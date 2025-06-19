// SA - pod_status.go
// This file provides a thread-safe cache for pod statuses in a Kubernetes environment.
// It allows setting, getting, and retrieving pod statuses by function name or all pods.
// It also includes the pod's status, IP address, function name, and namespace.

package k8s

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"                   // SA - Importing the corev1 package for Kubernetes core API interactions
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // SA - Importing the metav1 package for Kubernetes API interactions
	"k8s.io/client-go/kubernetes"                 // SA - Importing the Kubernetes client package
)

// PodStatus represents the status of a pod
type PodStatus struct {
	PodName           string    // The name of the pod
	Status            string    // "busy" or "idle"
	Timestamp         time.Time // When the status was last updated
	PodIP             string    // The IP address of the pod
	Function          string    // The function name this pod belongs to
	Namespace         string    // The namespace of the pod
	ActiveConnections int       // The number of active connections to the pod - for "max_inflight" support
	MaxInflight       *int      // Optional: Maximum number of inflight requests for this pod
}

// PodStatusCache provides a thread-safe cache for pod status
type PodStatusCache struct {
	cache     sync.Map              // Maps podName-IP -> PodStatus
	podLocks  sync.Map              // Maps podName-IP -> *sync.Mutex for per-pod locking
	clientset *kubernetes.Clientset // Optional: Kubernetes client for interacting with the API
}

// NewPodStatusCache creates a new pod status cache
func NewPodStatusCache() *PodStatusCache {
	return &PodStatusCache{
		cache: sync.Map{},
	}
}

// createKey creates a composite key from podName and podIP
func (p *PodStatusCache) createKey(podName, podIP string) string {
	return podName + "-" + podIP
}

func (p *PodStatusCache) TryMarkPodBusy(podName, podIP string) bool {
	key := p.createKey(podName, podIP)
	// Lock per-pod (using a sync.Map of mutexes)
	// Generates the key.
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	// Retrieves or creates a mutex for this pod.
	lock := lockIface.(*sync.Mutex)
	// Locks the mutex to ensure atomicity for this pod’s status update.
	lock.Lock()
	defer lock.Unlock()

	status, ok := p.cache.Load(key)
	if !ok {
		log.Printf("Pod %s not found in cache", key)
		return false
	}
	podStatus := status.(PodStatus)
	if podStatus.Status == "busy" || (podStatus.MaxInflight != nil && podStatus.ActiveConnections >= *podStatus.MaxInflight) {
		return false // Already busy or at limit
	}
	// podStatus.ActiveConnections++
	// if podStatus.MaxInflight != nil && podStatus.ActiveConnections >= *podStatus.MaxInflight {
	// 	podStatus.Status = "busy"
	// }
	// p.cache.Store(key, podStatus)
	return true
}

// Set updates the status of a pod
func (p *PodStatusCache) Set(podName, status, podIP, function, namespace string, maxInflight *int) {
	key := p.createKey(podName, podIP)

	// If the "status" is "busy", we update the active connections count by +1
	// If the "status" is "idle", we update the active connections count by -1
	// If the status is neither, we keep the current count or default to 0
	var (
		activeConnections int
		finalStatus       string
	)

	// Generates the key.
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	// Retrieves or creates a mutex for this pod.
	lock := lockIface.(*sync.Mutex)
	// Locks the mutex to ensure atomicity for this pod’s status update.
	lock.Lock()
	defer lock.Unlock()

	// If value is found in the cache, we update it
	if value, exists := p.cache.Load(key); exists {
		current := value.(PodStatus)
		if current.MaxInflight == nil {
			current.MaxInflight = maxInflight
		}
		if status == "busy" {
			activeConnections = current.ActiveConnections + 1
			if activeConnections >= *current.MaxInflight {
				finalStatus = "busy"
			} else {
				finalStatus = "idle" // If not at max inflight, we consider it idle
			}
		} else if status == "idle" {
			activeConnections = max(current.ActiveConnections-1, 0)
			finalStatus = "idle"
		} else {
			activeConnections = current.ActiveConnections
			finalStatus = status
		}
	}
	// If value is not found in the cache, we create a new entry
	if _, exists := p.cache.Load(key); !exists {
		activeConnections = 0
		if activeConnections >= *maxInflight {
			finalStatus = "busy"
		} else {
			finalStatus = "idle" // If not at max inflight, we consider it idle
		}
	}

	log.Printf("Setting pod status: %s, IP: %s, Function: %s, Namespace: %s, Status: %s, ActiveConnections: %d",
		podName, podIP, function, namespace, finalStatus, activeConnections)
	p.cache.Store(key, PodStatus{
		Status:            finalStatus,
		Timestamp:         time.Now(),
		PodIP:             podIP,
		Function:          function,
		Namespace:         namespace,
		PodName:           podName,
		ActiveConnections: activeConnections,
		MaxInflight:       maxInflight,
	})
}

// Get retrieves the status of a pod by podName and podIP
func (p *PodStatusCache) Get(podName, podIP string) (PodStatus, bool) {
	key := p.createKey(podName, podIP)
	value, exists := p.cache.Load(key)
	if !exists {
		return PodStatus{}, false
	}
	return value.(PodStatus), true
}

// GetByPodName retrieves the status of a pod by podName only
func (p *PodStatusCache) GetByPodName(podName string) []PodStatus {
	result := []PodStatus{}

	p.cache.Range(func(key, value interface{}) bool {
		keyStr := key.(string)
		if len(keyStr) > len(podName) && keyStr[:len(podName)] == podName && keyStr[len(podName)] == '-' {
			result = append(result, value.(PodStatus))
		}
		return true
	})

	return result
}

// Add a function-level lock mechanism using the existing podLocks
func (p *PodStatusCache) getFunctionLock(function, namespace string) *sync.Mutex {
	key := "function-" + function + "-" + namespace
	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
	return lockIface.(*sync.Mutex)
}

// GetByFunction returns all pods for a specific function
func (p *PodStatusCache) GetByFunction(function, namespace string) []PodStatus {
	lock := p.getFunctionLock(function, namespace)
	lock.Lock()
	defer lock.Unlock()

	var result []PodStatus // making sure to use a copy of the slice

	p.cache.Range(func(key, value interface{}) bool {
		status := value.(PodStatus)
		if status.Function == function && status.Namespace == namespace {
			result = append(result, status)
		}
		return true
	})

	return result // This is a copy of the slice, not a reference for safe use
}

// GetAll returns all pod statuses
func (p *PodStatusCache) GetAll() map[string]PodStatus {
	result := make(map[string]PodStatus)

	p.cache.Range(func(key, value interface{}) bool {
		compositeKey := key.(string)
		status := value.(PodStatus)
		result[compositeKey] = status
		return true
	})

	return result
}

// GetByPodIP retrieves pod status by IP address
func (p *PodStatusCache) GetByPodIP(podIP string) []PodStatus {
	result := []PodStatus{}

	p.cache.Range(func(key, value interface{}) bool {
		status := value.(PodStatus)
		if status.PodIP == podIP {
			result = append(result, status)
		}
		return true
	})

	return result
}

// // SA - This function retrieves the global lock for the cache.
// // It ensures that only one operation can modify the cache at a time.
// func (p *PodStatusCache) getGlobalLock() *sync.Mutex {
// 	key := "global-cache-lock"
// 	lockIface, _ := p.podLocks.LoadOrStore(key, &sync.Mutex{})
// 	return lockIface.(*sync.Mutex)
// }

// Helper to refresh addresses from Kubernetes Endpoints
func refreshAddresses(functionName, namespace string, clientset *kubernetes.Clientset) []corev1.EndpointAddress {
	endpoints, err := clientset.CoreV1().Endpoints(namespace).Get(context.TODO(), functionName, metav1.GetOptions{})
	if err != nil || len(endpoints.Subsets) == 0 {
		return nil
	}
	var all []corev1.EndpointAddress
	for _, subset := range endpoints.Subsets {
		all = append(all, subset.Addresses...)
	}
	log.Printf("Refreshed addresses for function %s in namespace %s: %d addresses found", functionName, namespace, len(all))
	return all
}

// Prune the cache by removing old entries and keeping only the most recent status for each pod
func (c *PodStatusCache) PruneByAddresses(function, namespace string,
	clientset *kubernetes.Clientset, addresses *[]corev1.EndpointAddress,
	max_inflight int) {
	lock := c.getFunctionLock(function, namespace)
	lock.Lock()
	defer lock.Unlock()

	// 0. Refresh addresses from Kubernetes Endpoints
	validAddresses := refreshAddresses(function, namespace, clientset)
	if validAddresses == nil {
		log.Printf("Failed to refresh addresses for function %s in namespace %s", function, namespace)
	}
	if addresses == nil || len(*addresses) == 0 {
		// If no addresses are provided or the refresh failed, use the refreshed addresses
		addresses = &validAddresses
	}
	addrSet := make(map[string]corev1.EndpointAddress, len(*addresses))
	for _, addr := range *addresses {
		addrSet[addr.IP] = addr
	}
	// 1. Remove stale entries
	c.cache.Range(func(key, value interface{}) bool {
		pod := value.(PodStatus)
		if pod.Function == function && pod.Namespace == namespace {
			if _, ok := addrSet[pod.PodIP]; !ok {
				c.cache.Delete(key)
			}
		}
		return true
	})
	// 2. Add new endpoints as idle if not present
	for ip, addr := range addrSet {
		found := false
		c.cache.Range(func(key, value interface{}) bool {
			pod := value.(PodStatus)
			if pod.Function == function && pod.Namespace == namespace && pod.PodIP == ip {
				found = true
				return false // stop searching
			}
			return true
		})
		if !found {
			// Use addr.TargetRef.Name if available, else use IP as PodName
			podName := ip
			if addr.TargetRef != nil && addr.TargetRef.Name != "" {
				podName = addr.TargetRef.Name
			}
			c.Set(podName, "idle", ip, function, namespace, &max_inflight)
		}
	}
}
