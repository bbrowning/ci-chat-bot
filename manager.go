package main

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bbrowning/crc-cluster-bot/pkg/crc"
	crcv1alpha1 "github.com/bbrowning/crc-operator/pkg/apis/crc/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog"
)

const (
	// maxTotalClusters limits the number of simultaneous clusters across all users to
	// prevent saturating the infrastructure account.
	maxTotalClusters = 3
)

// ClusterRequest keeps information about the request a user made to create
// a cluster. This is reconstructable from a CrcCluster.
type ClusterRequest struct {
	OriginalMessage string

	User string

	// Bundle is the name of the CRC bundle
	Bundle string

	Channel     string
	RequestedAt time.Time
	Name        string

	JobName string
	Params  map[string]string
}

// JobManager responds to user actions and tracks the state of the launched
// clusters.
type JobManager interface {
	SetNotifier(JobCallbackFunc)

	LaunchClusterForUser(req *ClusterRequest) (string, error)
	SyncClusterForUser(user string) (string, error)
	TerminateClusterForUser(user string) (string, error)
	GetLaunchCluster(user string) (*Job, error)
	LookupBundle(bundle string) (string, error)
	ListBundles() ([]string, error)
	ListClusters(users ...string) string
}

// JobCallbackFunc is invoked when the job changes state in a significant
// way.
type JobCallbackFunc func(Job)

// Job responds to user requests and tracks the state of the launched
// jobs. This object must be recreatable from a ProwJob, but the RequestedChannel
// field may be empty to indicate the user has already been notified.
type Job struct {
	Name string

	OriginalMessage string

	Ready   bool
	JobName string

	Params map[string]string

	Mode string

	Bundle string

	Credentials     string
	PasswordSnippet string
	Failure         string

	RequestedBy      string
	RequestedChannel string

	RequestedAt   time.Time
	ExpiresAt     time.Time
	StartDuration time.Duration
	Complete      bool
}

func (j Job) IsComplete() bool {
	return j.Complete || len(j.Credentials) > 0 || j.Ready
}

type jobManager struct {
	lock                 sync.Mutex
	requests             map[string]*ClusterRequest
	jobs                 map[string]*Job
	started              time.Time
	recentStartEstimates []time.Duration

	clusterPrefix string
	maxClusters   int
	maxAge        time.Duration

	pullSecret          string
	crcBundleClient     dynamic.NamespaceableResourceInterface
	crcClusterClient    dynamic.NamespaceableResourceInterface
	crcBundleNamespace  string
	crcClusterNamespace string

	muJob struct {
		lock    sync.Mutex
		running map[string]struct{}
	}

	notifierFn JobCallbackFunc
}

// NewJobManager creates a manager that will track the requests made by a user to create clusters
// and reflect that state into ProwJobs that launch clusters. It attempts to recreate state on startup
// by querying prow, but does not guarantee that some notifications to users may not be sent or may be
// sent twice.
func NewJobManager(pullSecret string, crcBundleClient dynamic.NamespaceableResourceInterface, crcClusterClient dynamic.NamespaceableResourceInterface) *jobManager {
	m := &jobManager{
		requests:      make(map[string]*ClusterRequest),
		jobs:          make(map[string]*Job),
		clusterPrefix: "bot-",
		maxClusters:   maxTotalClusters,
		maxAge:        5 * time.Hour,

		pullSecret:          pullSecret,
		crcBundleClient:     crcBundleClient,
		crcClusterClient:    crcClusterClient,
		crcBundleNamespace:  "crc-operator",
		crcClusterNamespace: "crc-clusters",
	}
	m.muJob.running = make(map[string]struct{})
	return m
}

func (m *jobManager) Start() error {
	go wait.Forever(func() {
		if err := m.sync(); err != nil {
			klog.Infof("error during sync: %v", err)
			return
		}
		time.Sleep(3 * time.Minute)
	}, time.Minute)
	return nil
}

func paramsFromAnnotation(value string) (map[string]string, error) {
	values := make(map[string]string)
	if len(value) == 0 {
		return values, nil
	}
	for _, part := range strings.Split(value, ",") {
		if len(part) == 0 {
			return nil, fmt.Errorf("parameter may not be empty")
		}
		parts := strings.SplitN(part, "=", 2)
		key := strings.TrimSpace(parts[0])
		if len(key) == 0 {
			return nil, fmt.Errorf("parameter name may not be empty")
		}
		if len(parts) == 1 {
			values[key] = ""
			continue
		}
		values[key] = parts[1]
	}
	return values, nil
}

func paramsToString(params map[string]string) string {
	var pairs []string
	for k, v := range params {
		if len(k) == 0 {
			continue
		}
		if len(v) == 0 {
			pairs = append(pairs, k)
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (m *jobManager) sync() error {
	u, err := m.crcClusterClient.Namespace(m.crcClusterNamespace).List(metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			"crc-cluster-bot.openshift.io/launch": "true",
		}).String(),
	})
	if err != nil {
		return err
	}
	list := &crcv1alpha1.CrcClusterList{}
	if err := crc.UnstructuredToObject(u, list); err != nil {
		return err
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	now := time.Now()
	if m.started.IsZero() {
		m.started = now
	}

	var clusterNamesToStop []string
	for _, cluster := range list.Items {
		klog.Infof("Found cluster: %s", cluster.Name)
		previous := m.jobs[cluster.Name]

		j := &Job{
			Name:             cluster.Name,
			Ready:            cluster.Status.Conditions.IsTrueFor("Ready"),
			Bundle:           cluster.Spec.BundleName,
			OriginalMessage:  cluster.Annotations["crc-cluster-bot.openshift.io/originalMessage"],
			RequestedBy:      cluster.Annotations["crc-cluster-bot.openshift.io/user"],
			RequestedChannel: cluster.Annotations["crc-cluster-bot.openshift.io/channel"],
			RequestedAt:      cluster.CreationTimestamp.Time,
		}

		var err error
		j.Params, err = paramsFromAnnotation(cluster.Annotations["crc-cluster-bot.openshift.io/params"])
		if err != nil {
			klog.Infof("Unable to unmarshal parameters from %s: %v", cluster.Name, err)
			continue
		}

		if expirationString := cluster.Annotations["crc-cluster-bot.openshift.io/expires"]; len(expirationString) > 0 {
			if maxSeconds, err := strconv.Atoi(expirationString); err == nil && maxSeconds > 0 {
				j.ExpiresAt = cluster.CreationTimestamp.Add(time.Duration(maxSeconds) * time.Second)
			}
		}
		if j.ExpiresAt.IsZero() {
			j.ExpiresAt = cluster.CreationTimestamp.Time.Add(m.maxAge)
		}

		if j.ExpiresAt.Before(now) {
			clusterNamesToStop = append(clusterNamesToStop, j.Name)
		}

		if j.Ready {
			j.Failure = ""
			if err := populateClusterCredentials(j, &cluster); err != nil {
				return err
			}
		}

		if user := j.RequestedBy; len(user) > 0 {
			if _, ok := m.requests[user]; !ok {
				params, err := paramsFromAnnotation(cluster.Annotations["crc-cluster-bot.openshift.io/params"])
				if err != nil {
					klog.Infof("Unable to unmarshal parameters from %s: %v", cluster.Name, err)
					continue
				}
				m.requests[user] = &ClusterRequest{
					OriginalMessage: cluster.Annotations["crc-cluster-bot.openshift.io/originalMessage"],

					User:        user,
					Name:        cluster.Name,
					Params:      params,
					RequestedAt: cluster.CreationTimestamp.Time,
					Channel:     cluster.Annotations["crc-cluster-bot.openshift.io/channel"],
				}
			}
		}

		m.jobs[cluster.Name] = j
		if previous == nil || previous.Ready != j.Ready || !previous.IsComplete() {
			go m.handleClusterStartup(*j, "sync")
		}
	}

	// actually terminate too old clusters
	for _, clusterName := range clusterNamesToStop {
		if err := m.stopCluster(clusterName); err != nil {
			klog.Errorf("unable to terminate running cluster %s: %v", clusterName, err)
			return err
		}
	}

	// forget everything that is too old
	for _, cluster := range m.jobs {
		if cluster.ExpiresAt.Before(now) {
			klog.Infof("cluster %q is expired", cluster.Name)
			delete(m.jobs, cluster.Name)
		}
	}
	for _, req := range m.requests {
		if req.RequestedAt.Add(m.maxAge * 2).Before(now) {
			klog.Infof("request %q is expired", req.User)
			delete(m.requests, req.User)
		}
	}

	klog.Infof("Cluster sync complete, %d clusters and %d requests", len(m.jobs), len(m.requests))
	return nil
}

func (m *jobManager) SetNotifier(fn JobCallbackFunc) {
	m.lock.Lock()
	defer m.lock.Unlock()
	m.notifierFn = fn
}

func (m *jobManager) estimateCompletion(requestedAt time.Time) time.Duration {
	// find the median, or default to 15m
	var median time.Duration
	if l := len(m.recentStartEstimates); l > 0 {
		median = m.recentStartEstimates[l/2]
	}
	if median < time.Minute {
		median = 15 * time.Minute
	}

	if requestedAt.IsZero() {
		return median.Truncate(time.Second)
	}

	lastEstimate := median - time.Now().Sub(requestedAt)
	if lastEstimate < 0 {
		return time.Minute
	}
	return lastEstimate.Truncate(time.Second)
}

func contains(arr []string, s string) bool {
	for _, item := range arr {
		if s == item {
			return true
		}
	}
	return false
}

func (m *jobManager) ListClusters(users ...string) string {
	m.lock.Lock()
	defer m.lock.Unlock()

	var clusters []*Job
	var runningClusters int
	for _, cluster := range m.jobs {
		clusters = append(clusters, cluster)
		runningClusters++
	}
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].RequestedAt.Before(clusters[j].RequestedAt) {
			return true
		}
		if clusters[i].Name < clusters[j].Name {
			return true
		}
		return false
	})

	buf := &bytes.Buffer{}
	now := time.Now()
	if len(clusters) == 0 {
		fmt.Fprintf(buf, "No clusters up (start time is approximately %d minutes):\n\n", m.estimateCompletion(time.Time{})/time.Minute)
	} else {
		fmt.Fprintf(buf, "%d/%d clusters up (start time is approximately %d minutes):\n\n", runningClusters, m.maxClusters, m.estimateCompletion(time.Time{})/time.Minute)
		for _, cluster := range clusters {
			var details string

			// summarize the job parameters
			var options string
			params := make(map[string]string)
			for k, v := range cluster.Params {
				params[k] = v
			}
			if s := paramsToString(params); len(s) > 0 {
				options = fmt.Sprintf(" (%s)", s)
			}

			bundle := cluster.Bundle
			switch {
			case cluster.Complete:
				fmt.Fprintf(buf, "• <@%s>%s%s - cluster has requested shut down%s\n", cluster.RequestedBy, bundle, options, details)
			case len(cluster.Credentials) > 0:
				fmt.Fprintf(buf, "• <@%s>%s%s - available and will be torn down in %d minutes%s\n", cluster.RequestedBy, bundle, options, int(cluster.ExpiresAt.Sub(now)/time.Minute), details)
			case len(cluster.Failure) > 0:
				fmt.Fprintf(buf, "• <@%s>%s%s - failure: %s%s\n", cluster.RequestedBy, bundle, options, cluster.Failure, details)
			default:
				fmt.Fprintf(buf, "• <@%s>%s%s - starting, %d minutes elapsed%s\n", cluster.RequestedBy, bundle, options, int(now.Sub(cluster.RequestedAt)/time.Minute), details)
			}
		}
		fmt.Fprintf(buf, "\n")
	}

	fmt.Fprintf(buf, "\nbot uptime is %.1f minutes", now.Sub(m.started).Seconds()/60)
	return buf.String()
}

type callbackFunc func(job Job)

func (m *jobManager) GetLaunchCluster(user string) (*Job, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	existing, ok := m.requests[user]
	if !ok {
		return nil, fmt.Errorf("you haven't requested a cluster or your cluster expired")
	}
	if len(existing.Name) == 0 {
		return nil, fmt.Errorf("you are still on the waitlist")
	}
	cluster, ok := m.jobs[existing.Name]
	if !ok {
		return nil, fmt.Errorf("your cluster has expired and credentials are no longer available")
	}
	copied := *cluster
	return &copied, nil
}

func (m *jobManager) LookupBundle(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("you must specify a bundle to lookup")
	}
	u, err := m.crcBundleClient.Namespace(m.crcBundleNamespace).Get(name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("could not lookup bundle %s: %v", name, err)
	}
	bundle := &crcv1alpha1.CrcBundle{}
	if err := crc.UnstructuredToObject(u, bundle); err != nil {
		return "", err
	}
	out := fmt.Sprintf(`
Bundle %s:
  image: %s,
  diskSize: %s
`, bundle.Name, bundle.Spec.Image, bundle.Spec.DiskSize)
	return out, nil
}

func (m *jobManager) ListBundles() ([]string, error) {
	var bundles []string
	u, err := m.crcBundleClient.Namespace(m.crcBundleNamespace).List(metav1.ListOptions{})
	if err != nil {
		return bundles, fmt.Errorf("could not list bundles: %v", err)
	}
	bundleList := &crcv1alpha1.CrcBundleList{}
	if err := crc.UnstructuredToObject(u, bundleList); err != nil {
		return bundles, err
	}
	for _, bundle := range bundleList.Items {
		bundles = append(bundles, bundle.Name)
	}
	return bundles, nil
}

func (m *jobManager) resolveToCluster(req *ClusterRequest) (*Job, error) {
	user := req.User
	if len(user) == 0 {
		return nil, fmt.Errorf("must specify the name of the user who requested this cluster")
	}

	req.RequestedAt = time.Now()
	name := fmt.Sprintf("%s%s", m.clusterPrefix, namespaceSafeHash(req.RequestedAt.UTC().Format("2006-01-02-150405.9999")))
	req.Name = name

	cluster := &Job{
		OriginalMessage: req.OriginalMessage,
		Name:            name,

		Bundle: req.Bundle,
		Params: req.Params,

		RequestedBy:      user,
		RequestedChannel: req.Channel,
		RequestedAt:      req.RequestedAt,

		ExpiresAt: req.RequestedAt.Add(m.maxAge),
	}

	return cluster, nil
}

func (m *jobManager) LaunchClusterForUser(req *ClusterRequest) (string, error) {
	cluster, err := m.resolveToCluster(req)
	if err != nil {
		return "", err
	}

	klog.Infof("Cluster %q requested by user %q - params=%s", cluster.Name, req.User, paramsToString(cluster.Params))

	msg, err := func() (string, error) {
		m.lock.Lock()
		defer m.lock.Unlock()

		user := req.User
		existing, ok := m.requests[user]
		if ok {
			if len(existing.Name) == 0 {
				klog.Infof("user %q already requested cluster", user)
				return "", fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(existing.RequestedAt)/time.Minute)
			}
			if cluster, ok := m.jobs[existing.Name]; ok {
				if len(cluster.Credentials) > 0 {
					klog.Infof("user %q cluster is already up", user)
					return "your cluster is already running, see your credentials again with the 'auth' command", nil
				}
				if len(cluster.Failure) == 0 {
					klog.Infof("user %q cluster has no credentials yet", user)
					return "", fmt.Errorf("you have already requested a cluster and it should be ready in ~ %d minutes", m.estimateCompletion(existing.RequestedAt)/time.Minute)
				}

				klog.Infof("user %q cluster failed, allowing them to request another", user)
				delete(m.jobs, existing.Name)
				delete(m.requests, user)
			}
		}
		m.requests[user] = req

		launchedClusters := 0
		for _, cluster := range m.jobs {
			if cluster != nil && !cluster.Complete && len(cluster.Failure) == 0 {
				launchedClusters++
			}
		}
		if launchedClusters >= m.maxClusters {
			klog.Infof("user %q is will have to wait", user)
			var waitUntil time.Time
			for _, c := range m.jobs {
				if c == nil {
					continue
				}
				if waitUntil.Before(c.ExpiresAt) {
					waitUntil = c.ExpiresAt
				}
			}
			minutes := waitUntil.Sub(time.Now()).Minutes()
			if minutes < 1 {
				return "", fmt.Errorf("no clusters are currently available, unable to estimate when next cluster will be free")
			}
			return "", fmt.Errorf("no clusters are currently available, next slot available in %d minutes", int(math.Ceil(minutes)))
		}
		m.jobs[cluster.Name] = cluster
		klog.Infof("Cluster %q starting for %q", cluster.Name, user)
		return "", nil
	}()
	if err != nil || len(msg) > 0 {
		return msg, err
	}

	if err := m.newCluster(cluster); err != nil {
		return "", fmt.Errorf("the requested cluster cannot be started: %v", err)
	}

	go m.handleClusterStartup(*cluster, "start")

	return "", fmt.Errorf("a cluster is being created - I'll send you the credentials in about %d minutes", m.estimateCompletion(req.RequestedAt)/time.Minute)
}

func (m *jobManager) clusterNameForUser(user string) (string, error) {
	if len(user) == 0 {
		return "", fmt.Errorf("must specify the name of the user who requested this cluster")
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	existing, ok := m.requests[user]
	if !ok || len(existing.Name) == 0 {
		return "", fmt.Errorf("no cluster has been requested by you")
	}
	return existing.Name, nil
}

func (m *jobManager) TerminateClusterForUser(user string) (string, error) {
	name, err := m.clusterNameForUser(user)
	if err != nil {
		return "", err
	}
	klog.Infof("user %q requests cluster %q to be terminated", user, name)
	if err := m.stopCluster(name); err != nil {
		klog.Errorf("unable to terminate running cluster %s: %v", name, err)
		return "", fmt.Errorf("unable to terminate your cluster")
	}

	// mark the cluster as failed, clear the request, and allow the user to launch again
	m.lock.Lock()
	defer m.lock.Unlock()
	existing, ok := m.requests[user]
	if !ok || existing.Name != name {
		return "", fmt.Errorf("another cluster was launched while trying to stop this cluster")
	}
	delete(m.requests, user)
	if job, ok := m.jobs[name]; ok {
		job.Failure = "deletion requested"
		job.ExpiresAt = time.Now().Add(5 * time.Minute)
		job.Complete = true
	}
	return "the cluster was flagged for shutdown, you may now launch another", nil
}

func (m *jobManager) SyncClusterForUser(user string) (string, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if len(user) == 0 {
		return "", fmt.Errorf("must specify the name of the user who requested this cluster")
	}

	existing, ok := m.requests[user]
	if !ok || len(existing.Name) == 0 {
		return "", fmt.Errorf("no cluster has been requested by you")
	}
	cluster, ok := m.jobs[existing.Name]
	if !ok {
		return "", fmt.Errorf("cluster hasn't been initialized yet, cannot refresh")
	}

	var msg string
	switch {
	case len(cluster.Failure) == 0 && len(cluster.Credentials) == 0:
		return "cluster is still being loaded, please be patient", nil
	case len(cluster.Failure) > 0:
		msg = fmt.Sprintf("cluster had previously been marked as failed, checking again: %s", cluster.Failure)
	case len(cluster.Credentials) > 0:
		msg = fmt.Sprintf("cluster had previously been marked as successful, checking again")
	}

	copied := *cluster
	copied.Failure = ""
	klog.Infof("user %q requests cluster %q to be refreshed", user, copied.Name)
	go m.handleClusterStartup(copied, "refresh")

	return msg, nil
}

func (m *jobManager) clusterStartupIsComplete(cluster *Job) bool {
	m.lock.Lock()
	defer m.lock.Unlock()
	current, ok := m.jobs[cluster.Name]
	if !ok {
		return false
	}
	if current.IsComplete() {
		cluster.Ready = current.Ready
		cluster.Complete = current.Complete
		return true
	}
	return false
}

func (m *jobManager) handleClusterStartup(cluster Job, source string) {
	if !m.tryClusterLaunch(cluster.Name) {
		klog.Infof("Cluster %q already starting (%s)", cluster.Name, source)
		return
	}
	defer m.finishClusterLaunch(cluster.Name)

	if err := m.waitForClusterLaunch(&cluster); err != nil {
		klog.Errorf("Cluster %q failed to launch (%s): %v", cluster.Name, source, err)
		cluster.Failure = err.Error()
	}
	m.finishedClusterLaunch(cluster)
}

func (m *jobManager) finishedClusterLaunch(cluster Job) {
	m.lock.Lock()
	defer m.lock.Unlock()

	// track the 10 most recent starts in sorted order
	if len(cluster.Credentials) > 0 && cluster.StartDuration > 0 {
		m.recentStartEstimates = append(m.recentStartEstimates, cluster.StartDuration)
		if len(m.recentStartEstimates) > 10 {
			m.recentStartEstimates = m.recentStartEstimates[:10]
		}
		sort.Slice(m.recentStartEstimates, func(i, j int) bool {
			return m.recentStartEstimates[i] < m.recentStartEstimates[j]
		})
	}

	if len(cluster.RequestedChannel) > 0 && len(cluster.RequestedBy) > 0 {
		klog.Infof("Cluster %q complete, notify %q", cluster.Name, cluster.RequestedBy)
		if m.notifierFn != nil {
			go m.notifierFn(cluster)
		}
	}

	// ensure we send no further notifications
	cluster.RequestedChannel = ""
	m.jobs[cluster.Name] = &cluster
}

func (m *jobManager) tryClusterLaunch(name string) bool {
	m.muJob.lock.Lock()
	defer m.muJob.lock.Unlock()

	_, ok := m.muJob.running[name]
	if ok {
		return false
	}
	m.muJob.running[name] = struct{}{}
	return true
}

func (m *jobManager) finishClusterLaunch(name string) {
	m.muJob.lock.Lock()
	defer m.muJob.lock.Unlock()

	delete(m.muJob.running, name)
}
