package main

import (
	"context"
	log "github.com/Sirupsen/logrus"
	vault "github.com/uswitch/vault-creds"
	"gopkg.in/alecthomas/kingpin.v2"
	"os"
	"os/signal"
	"text/template"
	"time"
)

var (
	vaultAddr           = kingpin.Flag("vault-addr", "Vault address, e.g. https://vault:8200").String()
	serviceAccountToken = kingpin.Flag("token-file", "Service account token path").Default("/var/run/secrets/kubernetes.io/serviceaccount/token").String()
	loginPath           = kingpin.Flag("login-path", "Vault path to authenticate against").Required().String()
	authRole            = kingpin.Flag("auth-role", "Kubernetes authentication role").Required().String()
	secretPath          = kingpin.Flag("secret-path", "Path to secret in Vault. eg. database/creds/foo").Required().String()
	caCert              = kingpin.Flag("ca-cert", "Path to CA certificate to validate Vault server").String()

	templateFile = kingpin.Flag("template", "Path to template file").ExistingFile()
	out          = kingpin.Flag("out", "Output file name").String()

	renewInterval = kingpin.Flag("renew-interval", "Interval to renew credentials").Default("15m").Duration()
	leaseDuration = kingpin.Flag("lease-duration", "Credentials lease duration").Default("1h").Duration()

	jsonOutput = kingpin.Flag("json-log", "Output log in JSON format").Default("false").Bool()

	completedPath = kingpin.Flag("completed-path", "Path where a 'completion' file will be dropped").Default("/tmp/vault-creds/completed").String()
)

var (
	SHA = ""
)

func main() {
	kingpin.Parse()

	if *jsonOutput {
		log.SetFormatter(&log.JSONFormatter{})
	}

	logger := log.WithFields(log.Fields{"gitSHA": SHA})
	logger.Infof("started application")

	t, err := template.ParseFiles(*templateFile)
	if err != nil {
		log.Fatal("error opening template:", err)
	}

	vaultConfig := &vault.VaultConfig{
		VaultAddr: *vaultAddr,
		TLS:       &vault.TLSConfig{CACert: *caCert},
	}
	kubernetesConfig := &vault.KubernetesAuthConfig{
		TokenFile: *serviceAccountToken,
		LoginPath: *loginPath,
		Role:      *authRole,
	}

	factory := vault.NewKubernetesAuthClientFactory(vaultConfig, kubernetesConfig)
	client, authSecret, err := factory.Create()
	if err != nil {
		log.Fatal("error creating client:", err)
	}

	credsProvider := vault.NewCredentialsProvider(client, *secretPath)
	creds, err := credsProvider.Fetch()

	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	leaseManager := vault.NewLeaseManager(client)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		log.Printf("renewing %s lease every %s", *leaseDuration, *renewInterval)
		ticks := time.Tick(*renewInterval)
		for {
			select {
			case <-ctx.Done():
				log.Infof("stopping renewal")
				return
			case <-ticks:
				err := leaseManager.RenewAuthToken(ctx, authSecret.Auth.ClientToken, *leaseDuration)
				if err != nil {
					log.Errorf("error renewing auth: %s", err)
				}
				err = leaseManager.RenewSecret(ctx, creds.Secret, *leaseDuration)
				if err != nil {
					log.Errorf("error renewing db credentials: %s", err)
				}
			default:
				if _, err := os.Stat(*completedPath); err == nil {
					log.Infof("received completion signal")
					c <- os.Interrupt
				}
			}
		}
	}()

	if *out != "" {
		file, err := os.OpenFile(*out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		t.Execute(file, creds)
		log.Printf("wrote credentials to %s", file.Name())
	} else {
		t.Execute(os.Stdout, creds)
	}

	<-c
	log.Infof("shutting down")
	cancel()
}
