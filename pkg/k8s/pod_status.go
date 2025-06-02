// SA - pod_status.go
// This file provides a thread-safe cache for pod statuses in a Kubernetes environment.
// It allows setting, getting, and retrieving pod statuses by function name or all pods.
// It also includes the pod's status, IP address, function name, and namespace.

package k8s

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// PodStatus represents the status of a pod
type PodStatus struct {
	PodName   string    // The name of the pod
	Status    string    // "busy" or "idle"
	Timestamp time.Time // When the status was last updated
	PodIP     string    // The IP address of the pod
	Function  string    // The function name this pod belongs to
	Namespace string    // The namespace of the pod
}

// PodStatusCache provides a thread-safe cache for pod status
type PodStatusCache struct {
	cache sync.Map // Maps podName-IP -> PodStatus
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

// Set updates the status of a pod
func (p *PodStatusCache) Set(podName, status, podIP, function, namespace string) {
	key := p.createKey(podName, podIP)
	p.cache.Store(key, PodStatus{
		Status:    status,
		Timestamp: time.Now(),
		PodIP:     podIP,
		Function:  function,
		Namespace: namespace,
		PodName:   podName,
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

// GetByFunction returns all pods for a specific function
func (p *PodStatusCache) GetByFunction(function, namespace string) []PodStatus {
	result := []PodStatus{}

	p.cache.Range(func(key, value interface{}) bool {
		status := value.(PodStatus)
		if status.Function == function && status.Namespace == namespace {
			result = append(result, status)
		}
		return true
	})

	return result
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

// Prune the cache by removing old entries and keeping only the most recent status for each pod
func (c *PodStatusCache) PruneByAddresses(function, namespace string, addresses []corev1.EndpointAddress) {
	addrSet := make(map[string]corev1.EndpointAddress, len(addresses))
	for _, addr := range addresses {
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
			c.Set(podName, "idle", ip, function, namespace)
		}
	}
}
