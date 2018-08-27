// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/howeyc/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"istio.io/istio/pilot/pkg/kube/inject"
	"istio.io/istio/pkg/cmd"
	"istio.io/istio/pkg/collateral"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/probe"
	"istio.io/istio/pkg/util"
	"istio.io/istio/pkg/version"
)

var (
	flags = struct {
		loggingOptions *log.Options

		meshconfig          string
		injectConfigFile    string
		certFile            string
		privateKeyFile      string
		caCertFile          string
		port                int
		healthCheckInterval time.Duration
		healthCheckFile     string
		probeOptions        probe.Options
		kubeconfigFile      string
		webhookConfigName   string
		webhookName         string
	}{
		loggingOptions: log.DefaultOptions(),
	}

	rootCmd = &cobra.Command{
		Use:          "sidecar-injector",
		Short:        "Kubernetes webhook for automatic Istio sidecar injection.",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(0),
		RunE: func(c *cobra.Command, _ []string) error {
			cmd.PrintFlags(c.Flags())
			if err := log.Configure(flags.loggingOptions); err != nil {
				return err
			}

			log.Infof("version %s", version.Info.String())

			parameters := inject.WebhookParameters{
				ConfigFile:          flags.injectConfigFile,
				MeshFile:            flags.meshconfig,
				CertFile:            flags.certFile,
				KeyFile:             flags.privateKeyFile,
				Port:                flags.port,
				HealthCheckInterval: flags.healthCheckInterval,
				HealthCheckFile:     flags.healthCheckFile,
			}
			wh, err := inject.NewWebhook(parameters)
			if err != nil {
				return multierror.Prefix(err, "failed to create injection webhook")
			}

			if err := patchCertLoop(); err != nil {
				return multierror.Prefix(err, "failed to start patch cert loop")
			}

			stop := make(chan struct{})
			go wh.Run(stop)
			cmd.WaitSignal(stop)
			return nil
		},
	}

	probeCmd = &cobra.Command{
		Use:   "probe",
		Short: "Check the liveness or readiness of a locally-running server",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !flags.probeOptions.IsValid() {
				return errors.New("some options are not valid")
			}
			if err := probe.NewFileClient(&flags.probeOptions).GetStatus(); err != nil {
				return fmt.Errorf("fail on inspecting path %s: %v", flags.probeOptions.Path, err)
			}
			fmt.Println("OK")
			return nil
		},
	}
)

// patchCertLoop continually reapplies the caBundle PEM. This is required because it can be overwritten with empty
// values if the original yaml is reapplied (https://github.com/istio/istio/issues/6069).
// TODO(https://github.com/istio/istio/issues/6451) - only patch when caBundle changes
func patchCertLoop() error {
	client, err := kube.CreateClientset(flags.kubeconfigFile, "")
	if err != nil {
		return err
	}

	caCertPem, err := ioutil.ReadFile(flags.caCertFile)
	if err != nil {
		return err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	watchDir, _ := filepath.Split(flags.caCertFile)
	if err = watcher.Watch(watchDir); err != nil {
		return fmt.Errorf("could not watch %v: %v", flags.caCertFile, err)
	}

	go func() {
		tickerC := time.NewTicker(time.Second).C
		for {
			select {
			case <-tickerC:
				if err = util.PatchMutatingWebhookConfig(client.AdmissionregistrationV1beta1().MutatingWebhookConfigurations(),
					flags.webhookConfigName, flags.webhookName, caCertPem); err != nil {
					log.Errorf("Patch webhook failed: %s", err)
				}

			case <-watcher.Event:
				if b, err := ioutil.ReadFile(flags.caCertFile); err == nil {
					caCertPem = b
				} else {
					log.Errorf("CA bundle file read error: %v", err)
				}
			}
		}
	}()

	return nil
}

func init() {
	rootCmd.PersistentFlags().StringVar(&flags.meshconfig, "meshConfig", "/etc/istio/config/mesh",
		"File containing the Istio mesh configuration")
	rootCmd.PersistentFlags().StringVar(&flags.injectConfigFile, "injectConfig", "/etc/istio/inject/config",
		"File containing the Istio sidecar injection configuration and template")
	rootCmd.PersistentFlags().StringVar(&flags.certFile, "tlsCertFile", "/etc/istio/certs/cert-chain.pem",
		"File containing the x509 Certificate for HTTPS.")
	rootCmd.PersistentFlags().StringVar(&flags.privateKeyFile, "tlsKeyFile", "/etc/istio/certs/key.pem",
		"File containing the x509 private key matching --tlsCertFile.")
	rootCmd.PersistentFlags().StringVar(&flags.caCertFile, "caCertFile", "/etc/istio/certs/root-cert.pem",
		"File containing the x509 Certificate for HTTPS.")
	rootCmd.PersistentFlags().IntVar(&flags.port, "port", 443, "Webhook port")

	rootCmd.PersistentFlags().DurationVar(&flags.healthCheckInterval, "healthCheckInterval", 0,
		"Configure how frequently the health check file specified by --healthCheckFile should be updated")
	rootCmd.PersistentFlags().StringVar(&flags.healthCheckFile, "healthCheckFile", "",
		"File that should be periodically updated if health checking is enabled")
	rootCmd.PersistentFlags().StringVar(&flags.kubeconfigFile, "kubeconfig", "",
		"Specifies path to kubeconfig file. This must be specified when not running inside a Kubernetes pod.")
	rootCmd.PersistentFlags().StringVar(&flags.webhookConfigName, "webhookConfigName", "istio-sidecar-injector",
		"Name of the mutatingwebhookconfiguration resource in Kubernetes.")
	rootCmd.PersistentFlags().StringVar(&flags.webhookName, "webhookName", "sidecar-injector.istio.io",
		"Name of the webhook entry in the webhook config.")
	// Attach the Istio logging options to the command.
	flags.loggingOptions.AttachCobraFlags(rootCmd)

	cmd.AddFlags(rootCmd)

	rootCmd.AddCommand(version.CobraCommand())
	rootCmd.AddCommand(collateral.CobraCommand(rootCmd, &doc.GenManHeader{
		Title:   "Istio Sidecar Injector",
		Section: "sidecar-injector CLI",
		Manual:  "Istio Sidecar Injector",
	}))

	probeCmd.PersistentFlags().StringVar(&flags.probeOptions.Path, "probe-path", "",
		"Path of the file for checking the availability.")
	probeCmd.PersistentFlags().DurationVar(&flags.probeOptions.UpdateInterval, "interval", 0,
		"Duration used for checking the target file's last modified time.")
	rootCmd.AddCommand(probeCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(-1)
	}
}
