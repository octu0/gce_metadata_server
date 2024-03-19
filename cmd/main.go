package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/golang/glog"
	"github.com/google/go-tpm/legacy/tpm2"
	mds "github.com/salrashid123/gce_metadata_server"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/impersonate"
)

var (
	bindInterface      = flag.String("interface", "127.0.0.1", "interface address to bind to")
	port               = flag.String("port", ":8080", "port...")
	useDomainSocket    = flag.String("domainsocket", "", "listen only on unix socket")
	serviceAccountFile = flag.String("serviceAccountFile", "", "serviceAccountFile...")
	configFile         = flag.String("configFile", "config.json", "config file")
	useImpersonate     = flag.Bool("impersonate", false, "Impersonate a service Account instead of using the keyfile")
	useFederate        = flag.Bool("federate", false, "Use Workload Identity Federation ADC")
	useTPM             = flag.Bool("tpm", false, "Use TPM to get access and id_token")
	tpmPath            = flag.String("tpm-path", "/dev/tpm0", "Path to the TPM device (character device or a Unix socket).")
	persistentHandle   = flag.Int("persistentHandle", 0x81008000, "Handle value")
)

func main() {

	flag.Parse()

	ctx := context.Background()

	glog.Infof("Starting GCP metadataserver")

	configData, err := os.ReadFile(*configFile)
	if err != nil {
		glog.Errorf("Error reading config data file: %v\n", err)
		os.Exit(-1)
	}

	claims := &mds.Claims{}
	err = json.Unmarshal(configData, claims)
	if err != nil {
		glog.Errorf("Error parsing json: %v\n", err)
		os.Exit(-1)
	}

	var creds *google.Credentials
	_, ok := claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"]
	if !ok {
		glog.Errorf("default service account must be set")
		os.Exit(-1)
	}

	if *useImpersonate {
		glog.Infoln("Using Service Account Impersonation")

		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Email,
			Scopes:          claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Scopes,
		})
		if err != nil {
			glog.Errorf("Unable to create Impersonated TokenSource %v ", err)
			os.Exit(1)
		}

		creds = &google.Credentials{
			TokenSource: ts,
		}
	} else if *useFederate {
		glog.Infoln("Using Workload Identity Federation")

		if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") == "" {
			glog.Error("GOOGLE_APPLICATION_CREDENTIAL must be set with --federate")
			os.Exit(1)
		}

		glog.Infof("Federation path: %s", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
		var err error
		creds, err = google.FindDefaultCredentials(ctx, claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Scopes...)
		if err != nil {
			glog.Errorf("Unable load federated credentials %v", err)
			os.Exit(1)
		}
	} else if *useTPM {
		glog.Infoln("Using TPM based token handle")

		if *persistentHandle == 0 {
			glog.Error("persistent handle must be specified")
			os.Exit(1)
		}
		// verify we actually have access to the TPM
		rwc, err := tpm2.OpenTPM(*tpmPath)
		if err != nil {
			glog.Errorf("can't open TPM %s: %v", *tpmPath, err)
			os.Exit(1)
		}
		err = rwc.Close()
		if err != nil {
			glog.Errorf("can't close TPM %s: %v", *tpmPath, err)
			os.Exit(1)
		}
	} else {

		glog.Infoln("Using serviceAccountFile for credentials")
		var err error
		data, err := os.ReadFile(*serviceAccountFile)
		if err != nil {
			glog.Errorf("Unable to read serviceAccountFile %v", err)
			os.Exit(1)
		}
		creds, err = google.CredentialsFromJSON(ctx, data, claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Scopes...)
		if err != nil {
			glog.Errorf("Unable to parse serviceAccountFile %v ", err)
			os.Exit(1)
		}

		if creds.ProjectID != claims.ComputeMetadata.V1.Project.ProjectID {
			glog.Warningf("Warning: ProjectID in config file [%s] does not match project from credentials [%s]", claims.ComputeMetadata.V1.Project.ProjectID, creds.ProjectID)
		}

		// compare the svc account email in the cred file vs the config file
		//       note json struct for the service account file isn't exported  https://github.com/golang/oauth2/blob/master/google/google.go#L109
		// for now i'm parsing it directly
		credJsonMap := make(map[string](interface{}))
		err = json.Unmarshal(creds.JSON, &credJsonMap)
		if err != nil {
			glog.Errorf("Unable to parse serviceAccountFile as json %v ", err)
			os.Exit(1)
		}
		credFileEmail := credJsonMap["client_email"]
		if credFileEmail != claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Email {
			glog.Warningf("Warning: service account email in config file [%s] does not match project from credentials [%s]", claims.ComputeMetadata.V1.Instance.ServiceAccounts["default"].Email, credFileEmail)
		}

	}

	serverConfig := &mds.ServerConfig{
		BindInterface:    *bindInterface,
		Port:             *port,
		Impersonate:      *useImpersonate,
		Federate:         *useFederate,
		DomainSocket:     *useDomainSocket,
		UseTPM:           *useTPM,
		TPMPath:          *tpmPath,
		PersistentHandle: *persistentHandle,
	}

	f, err := mds.NewMetadataServer(ctx, serverConfig, creds, claims)
	if err != nil {
		glog.Errorf("Error creating metadata server %v\n", err)
		os.Exit(1)
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	err = f.Start()
	if err != nil {
		glog.Errorf("Error starting %v\n", err)
		os.Exit(1)
	}
	<-done
	err = f.Shutdown()
	if err != nil {
		glog.Errorf("Error stopping %v\n", err)
		os.Exit(1)
	}
}