// SA - pod_status.go
// This file provides a thread-safe cache for pod statuses in a Kubernetes environment.
// It allows setting, getting, and retrieving pod statuses by function name or all pods.
// It also includes the pod's status, IP address, function name, and namespace.

package k8s

import (
    "sync"
    "time"
)

// PodStatus represents the status of a pod
type PodStatus struct {
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

