package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog"
)

type options struct {
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	emptyFlags := flag.NewFlagSet("empty", flag.ContinueOnError)
	klog.InitFlags(emptyFlags)
	pflag.CommandLine.AddGoFlag(emptyFlags.Lookup("v"))
	pflag.Parse()
	klog.SetOutput(os.Stderr)

	botToken := os.Getenv("BOT_TOKEN")
	if len(botToken) == 0 {
		return fmt.Errorf("the environment variable BOT_TOKEN must be set")
	}

	pullSecret := os.Getenv("OPENSHIFT_PULL_SECRET")
	if len(pullSecret) == 0 {
		return fmt.Errorf("the environment variable OPENSHIFT_PULL_SECRET must be set")
	}

	maxClustersString := os.Getenv("MAX_CLUSTERS")
	if len(maxClustersString) == 0 {
		return fmt.Errorf("the environment variable MAX_CLUSTERS must be set")
	}
	maxClusters, err := strconv.Atoi(maxClustersString)
	if err != nil || maxClusters <= 0 {
		return fmt.Errorf("the environment variable MAX_CLUSTERS must be set to an integer greater than 0")
	}

	crcKubeconfig, _, _, err := loadKubeconfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(crcKubeconfig)
	if err != nil {
		return fmt.Errorf("unable to create crc client: %v", err)
	}
	crcBundleClient := dynamicClient.Resource(schema.GroupVersionResource{Group: "crc.developer.openshift.io", Version: "v1alpha1", Resource: "crcbundles"})
	crcClusterClient := dynamicClient.Resource(schema.GroupVersionResource{Group: "crc.developer.openshift.io", Version: "v1alpha1", Resource: "crcclusters"})

	manager := NewClusterManager(pullSecret, maxClusters, crcBundleClient, crcClusterClient)
	if err := manager.Start(); err != nil {
		return fmt.Errorf("unable to load initial configuration: %v", err)
	}

	bot := NewBot(botToken)
	for {
		if err := bot.Start(manager); err != nil && !isRetriable(err) {
			log.Print(err)
			return err
		}
		time.Sleep(5 * time.Second)
	}
}
