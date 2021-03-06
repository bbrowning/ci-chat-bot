package main

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"github.com/bbrowning/crc-cluster-bot/pkg/crc"
	crcv1alpha1 "github.com/bbrowning/crc-operator/pkg/apis/crc/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// supportedParameters are the allowed parameter keys that can be passed to clusters
var supportedParameters = []string{"persistent", "no-monitoring"}

// stopCluster stops a cluster. If the cluster is not persistent or
// the delete param is true, this also deletes it. If this method
// returns nil, it is safe to consider the cluster released.
func (m *clusterManager) stopCluster(name string, shouldDelete bool) (bool, error) {
	uns, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	var crcCluster crcv1alpha1.CrcCluster
	if err := crc.UnstructuredToObject(uns, &crcCluster); err != nil {
		return false, err
	}
	deleted := false
	if crcCluster.Spec.Storage.Persistent && !shouldDelete {
		crcCluster.Spec.Stopped = true
		crcCluster.Annotations["crc-cluster-bot.openshift.io/expires"] = strconv.Itoa(int(m.maxStoppedAge.Seconds()))
		if _, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Update(crc.ObjectToUnstructured(&crcCluster), metav1.UpdateOptions{}); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	} else {
		if err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Delete(name, &metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
		deleted = true
	}
	return deleted, nil
}

// createOrUpdateCluster creates or updates a CrcCluster for running the
// provided cluster and exits.
func (m *clusterManager) createOrUpdateCrcCluster(cluster *Cluster) error {
	if !m.tryClusterLaunch(cluster.Name) {
		klog.Infof("Cluster %q already starting", cluster.Name)
		return nil
	}
	defer m.finishClusterLaunch(cluster.Name)

	if cluster.IsComplete() && len(cluster.PasswordSnippet) > 0 {
		return nil
	}

	launchDeadline := 30 * time.Minute

	crcCluster, err := crc.ClusterForConfig(cluster.Bundle, m.pullSecret, cluster.Params)
	if err != nil {
		return err
	}

	crcCluster.ObjectMeta = metav1.ObjectMeta{
		Name:      cluster.Name,
		Namespace: m.crcClusterNamespace,
		Annotations: map[string]string{
			"crc-cluster-bot.openshift.io/originalMessage": cluster.OriginalMessage,
			"crc-cluster-bot.openshift.io/params":          paramsToString(cluster.Params),
			"crc-cluster-bot.openshift.io/user":            cluster.RequestedBy,
			"crc-cluster-bot.openshift.io/channel":         cluster.RequestedChannel,
			"crc-cluster-bot.openshift.io/startedAt":       time.Now().UTC().Format(time.RFC3339),
		},
		Labels: map[string]string{
			"crc-cluster-bot.openshift.io/launch": "true",
		},
	}

	// set standard annotations and environment variables
	crcCluster.Annotations["crc-cluster-bot.openshift.io/expires"] = strconv.Itoa(int(m.maxAge.Seconds() + launchDeadline.Seconds()))

	_, err = m.crcClusterClient.Namespace(m.crcClusterNamespace).Create(crc.ObjectToUnstructured(crcCluster), metav1.CreateOptions{})
	if err != nil && errors.IsAlreadyExists(err) {
		uns, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Get(cluster.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var updatedCrcCluster crcv1alpha1.CrcCluster
		if err := crc.UnstructuredToObject(uns, &updatedCrcCluster); err != nil {
			return err
		}
		updatedCrcCluster.Annotations = crcCluster.Annotations
		updatedCrcCluster.Spec.Stopped = false
		if _, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Update(crc.ObjectToUnstructured(&updatedCrcCluster), metav1.UpdateOptions{}); err != nil {
			return err
		}
		crcCluster = &updatedCrcCluster
	} else {
		return err
	}

	return nil
}

func (m *clusterManager) waitForClusterLaunch(cluster *Cluster) error {
	if cluster.IsComplete() && len(cluster.PasswordSnippet) > 0 {
		return nil
	}

	klog.Infof("Waiting for cluster %q to launch in namespace %s", cluster.Name, m.crcClusterNamespace)
	var crcCluster *crcv1alpha1.CrcCluster
	err := wait.PollImmediate(30*time.Second, 40*time.Minute, func() (bool, error) {
		uns, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Get(cluster.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		var latestCrcCluster crcv1alpha1.CrcCluster
		if err := crc.UnstructuredToObject(uns, &latestCrcCluster); err != nil {
			return false, err
		}
		crcCluster = &latestCrcCluster

		done := crcCluster.Status.Conditions.IsTrueFor("Ready")
		return done, nil
	})
	if err != nil && err == wait.ErrWaitTimeout {
		return fmt.Errorf("cluster never became ready: %v", err)
	} else if err != nil {
		return err
	}

	started := cluster.RequestedAt

	if err := populateClusterCredentials(cluster, crcCluster); err != nil {
		return err
	}

	created := len(crcCluster.Annotations["crc-cluster-bot.openshift.io/expires"]) == 0
	startDuration := time.Now().Sub(started)
	m.clearNotificationAnnotations(cluster, created, startDuration)

	return nil
}

func populateClusterCredentials(cluster *Cluster, crcCluster *crcv1alpha1.CrcCluster) error {
	kubeconfigBytes, err := base64.StdEncoding.DecodeString(crcCluster.Status.Kubeconfig)
	if err != nil {
		return err
	}
	cluster.Credentials = string(kubeconfigBytes)
	cluster.PasswordSnippet = fmt.Sprintf(`
To access the cluster as the system:admin user when using 'oc', run 'export KUBECONFIG=/tmp/artifacts/installer/auth/kubeconfig'
Access the OpenShift web-console here: %s
Log in to the console with user kubeadmin and password %s
`, crcCluster.Status.ConsoleURL, crcCluster.Status.KubeAdminPassword)
	return nil
}

// clearNotificationAnnotations removes the channel notification annotations in case we crash,
// so we don't attempt to redeliver, and set the best estimate we have of the expiration time if we created the cluster
func (m *clusterManager) clearNotificationAnnotations(cluster *Cluster, created bool, startDuration time.Duration) {
	var patch []byte
	if created {
		patch = []byte(fmt.Sprintf(`{"metadata":{"annotations":{"crc-cluster-bot.openshift.io/channel":"","crc-cluster-bot.openshift.io/expires":"%d"}}}`, int(startDuration.Seconds()+m.maxAge.Seconds())))
	} else {
		patch = []byte(`{"metadata":{"annotations":{"crc-cluster-bot.openshift.io/channel":""}}}`)
	}
	if _, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).Patch(cluster.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		klog.Infof("error: Cluster %q unable to clear channel annotation: %v", cluster.Name, err)
	}
}

// loadKubeconfig loads connection configuration
// for the cluster we're deploying to. We prefer to
// use in-cluster configuration if possible, but will
// fall back to using default rules otherwise.
func loadKubeconfig() (*rest.Config, string, bool, error) {
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	clusterConfig, err := cfg.ClientConfig()
	if err != nil {
		return nil, "", false, fmt.Errorf("could not load client configuration: %v", err)
	}
	ns, isSet, err := cfg.Namespace()
	if err != nil {
		return nil, "", false, fmt.Errorf("could not load client namespace: %v", err)
	}
	return clusterConfig, ns, isSet, nil
}

// oneWayEncoding can be used to encode hex to a 62-character set (0 and 1 are duplicates) for use in
// short display names that are safe for use in kubernetes as resource names.
var oneWayNameEncoding = base32.NewEncoding("bcdfghijklmnpqrstvwxyz0123456789").WithPadding(base32.NoPadding)

func namespaceSafeHash(values ...string) string {
	hash := sha256.New()

	// the inputs form a part of the hash
	for _, s := range values {
		hash.Write([]byte(s))
	}

	// Object names can't be too long so we truncate
	// the hash. This increases chances of collision
	// but we can tolerate it as our input space is
	// tiny.
	return oneWayNameEncoding.EncodeToString(hash.Sum(nil)[:4])
}
