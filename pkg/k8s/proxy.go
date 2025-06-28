// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved\.
// Copyright 2020 OpenFaaS Author(s)

package k8s

import (
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync"

	// SA - Add the logging package
	"log"

	corelister "k8s.io/client-go/listers/core/v1"
	// SA - Add the kuberentes package
	"k8s.io/client-go/kubernetes"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

func NewFunctionLookup(ns string, lister corelister.EndpointsLister) *FunctionLookup {
	cache := NewPodStatusCache() // SA - Initialize the shared PodStatusCache
	lookup := &FunctionLookup{
		DefaultNamespace: ns,
		EndpointLister:   lister,
		Listers:          map[string]corelister.EndpointsNamespaceLister{},
		lock:             sync.RWMutex{},
		// SA - Add the Round-Robin module
		rrSelector: NewRoundRobinSelector(), // Initialize the Round-Robin selector
		// SA - Add the PodStatusCache
		podStatusCache: cache, // Initialize the PodStatusCache

	}
	lookup.idleFirstSelector = NewIdleFirstSelector(nil, cache, lookup) // Initialize the IdleFirstSelector with the cache
	return lookup
}

type FunctionLookup struct {
	DefaultNamespace string
	EndpointLister   corelister.EndpointsLister
	Listers          map[string]corelister.EndpointsNamespaceLister

	lock sync.RWMutex

	// SA - Add the Round-Robin module
	rrSelector *RoundRobinSelector // Round-Robin selector for function endpoints
	// SA - Add the PodStatusCache
	podStatusCache *PodStatusCache // Cache for pod statuses
	// SA - Add the idle-first selector
	idleFirstSelector *IdleFirstSelector // IdleFirstSelector for function endpoints

	// // SA - Add the Round-Robin strategy last index tracker
	// // for each function-namespace combination.
	// rrLock sync.RWMutex // lock for rrLastSelected
	// rrLastSelected map[string]int // key: functionName.namespace, value: last index
}

func (f *FunctionLookup) GetLister(ns string) corelister.EndpointsNamespaceLister {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.Listers[ns]
}

func (f *FunctionLookup) SetLister(ns string, lister corelister.EndpointsNamespaceLister) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.Listers[ns] = lister
}

func getNamespace(name, defaultNamespace string) string {
	namespace := defaultNamespace
	if strings.Contains(name, ".") {
		namespace = name[strings.LastIndexAny(name, ".")+1:]
	}
	return namespace
}

// SA - Add the setter for clientset
func (f *FunctionLookup) SetIdleFirstSelectorClientset(clientset *kubernetes.Clientset) {
	if f.idleFirstSelector == nil {
		f.idleFirstSelector = NewIdleFirstSelector(clientset, f.podStatusCache, f)
		f.idleFirstSelector.podStatusCache.clientset = clientset // Set the clientset in the IdleFirstSelector
	} else {
		f.idleFirstSelector.clientset = clientset
		f.idleFirstSelector.podStatusCache.clientset = clientset // Update the clientset in the IdleFirstSelector's cache
	}
}

// SA - randomID generates a random ID for tracing purposes
// This function generates a random 16-byte ID and returns it as a hexadecimal string.
func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// This method resolves a function name to a URL.
// It extracts the function name and namespace from the provided name,
// verifies the namespace, and retrieves the corresponding service endpoints.
// If the service has no subsets or addresses, it returns an error.
// If successful, it randomly selects an address from the service's endpoints
// and constructs a URL pointing to the function's watchdog port (8080).
func (l *FunctionLookup) Resolve(name string) (url.URL, error) {
	// SA - Add a random ID for tracing
	requestID := randomID()
	functionName := name
	namespace := getNamespace(name, l.DefaultNamespace)
	if err := l.verifyNamespace(namespace); err != nil {
		return url.URL{}, err
	}

	if strings.Contains(name, ".") {
		functionName = strings.TrimSuffix(name, "."+namespace)
	}

	nsEndpointLister := l.GetLister(namespace)

	if nsEndpointLister == nil {
		l.SetLister(namespace, l.EndpointLister.Endpoints(namespace))

		nsEndpointLister = l.GetLister(namespace)
	}

	svc, err := nsEndpointLister.Get(functionName)
	if err != nil {
		return url.URL{}, fmt.Errorf("error listing \"%s.%s\": %s", functionName, namespace, err.Error())
	}

	if len(svc.Subsets) == 0 {
		return url.URL{}, fmt.Errorf("no subsets available for \"%s.%s\"", functionName, namespace)
	}

	// This is where the faas-netes controller
	// creates the service with a single subset i.e.
	// it looks for the available pods from the service/deployment
	// and forwards the request to one of them.
	// However, if there are no pods available, the faas-provider
	// interface handles the creation/scaling of the functions
	// when the request arrives.
	all := len(svc.Subsets[0].Addresses)
	if len(svc.Subsets[0].Addresses) == 0 {
		return url.URL{}, fmt.Errorf("no addresses in subset for \"%s.%s\"", functionName, namespace)
	}

	// target := rand.Intn(all) // Random selection of an address
	// SA - ToDo: 1. Round-Robin selection
	// key := functionName + "." + namespace
	// target = l.rrSelector.Next(key, all)
	// log.Printf("Selected target index %d for function %s in namespace %s", target, functionName, namespace)

	// var max_inflight int
	// SA - ToDo: 2. Idle-first selection
	target, err := l.idleFirstSelector.Select(
		svc.Subsets[0].Addresses,
		requestID, // SA - Pass the requestID for tracing
		functionName,
		namespace,
	)

	if err != nil {
		// Handle different types of queue/selection errors
		if strings.Contains(err.Error(), "timeout") {
			return url.URL{}, fmt.Errorf("[REQ:%s] service temporarily unavailable for \"%s.%s\": %v", requestID, functionName, namespace, err)
		} else if strings.Contains(err.Error(), "queue full") {
			return url.URL{}, fmt.Errorf("[REQ:%s] service overloaded for \"%s.%s\": %v", requestID, functionName, namespace, err)
		} else {
			return url.URL{}, fmt.Errorf("[REQ:%s] no available pods for \"%s.%s\": %v", requestID, functionName, namespace, err)
		}
	}
	if target < 0 || target >= all {
		return url.URL{}, fmt.Errorf("[REQ:%s] invalid target index %d for function %s in namespace %s", requestID, target, functionName, namespace)
	}

	// // SA - ToDo:
	// // Instead of randomly selecting an address,
	// // what other strategies could be used?
	// // 1. Round-robin selection
	// // 2. Least connections
	// // 3. Weighted distribution based on previous response times

	// // SA - 1. Round-robin selection
	// key := functionName + "." + namespace
	// l.rrLock.Lock()
	// if l.rrLastSelected == nil {
	// 	l.rrLastSelected = make(map[string]int) // Initialize the map if it doesn't exist
	// }
	// last := l.rrLastSelected[key]

	// // SA - ensure the last index is within the range of available addresses
	// if last >= all  || last < 0 {
	// 	// If last is out of bounds, reset it to 0
	// 	// This can happen if the service was updated or restarted
	// 	// and the last selected index is no longer valid.
	// 	// This ensures that we always start from the first address
	// 	last = 0
	// 	l.rrLastSelected[key] = last
	// }

	// next := (last + 1) % all
	// l.rrLastSelected[key] = next
	// l.rrLock.Unlock()
	// target = next
	// -------------------------------

	serviceIP := svc.Subsets[0].Addresses[target].IP

	podName := ""
	//SA - ToDo: Update the Pod StatusCache with the selected pod and its IP
	if targetRef := svc.Subsets[0].Addresses[target].TargetRef; targetRef != nil {
		podName = targetRef.Name
		// l.podStatusCache.Set(podName, "busy", serviceIP, functionName, namespace, &max_inflight) // Already marked busy in the idle-first selector - Duplicate
		log.Printf("[REQ:%s] Updated PodStatusCache for pod %s in function %s with IP %s as %s", requestID, podName, functionName, serviceIP, "BUSY")
	} else {
		log.Printf("[REQ:%s] No TargetRef found for address %s in function %s", requestID, serviceIP, functionName)
	}
	// ---------------------------------

	urlStr := fmt.Sprintf("http://%s:%d", serviceIP, watchdogPort)

	urlRes, err := url.Parse(urlStr)
	if err != nil {
		return url.URL{}, err
	}

	// SA - Add the function name and namespace to the URL query parameters
	if podName != "" {
		q := urlRes.Query()
		q.Set("podName", podName)
		q.Set("podIP", serviceIP)
		q.Set("podNamespace", namespace)
		q.Set("OpenFaaS-Internal-ID", requestID) // Add the request ID for tracing
		urlRes.RawQuery = q.Encode()
		log.Printf("[REQ:%s] Resolved URL for function %s in namespace %s: %s with pod %s and IP %s", requestID, functionName, namespace, urlRes.String(), podName, serviceIP)
	}

	return *urlRes, nil
}

func (l *FunctionLookup) verifyNamespace(name string) error {
	if name != "kube-system" {
		return nil
	}
	// ToDo use global namepace parse and validation
	return fmt.Errorf("namespace not allowed")
}

// SA - Add a method to get all pods for a specific function
// GetFunctionPodStatuses returns all pod statuses for a specific function in a namespace.
func (l *FunctionLookup) GetFunctionPodStatuses(function, namespace string) []PodStatus {
	return l.podStatusCache.GetByFunction(function, namespace)
}

// SA - Add a method to get all pod statuses
// GetAllPodStatuses returns all pod statuses from the cache.
func (l *FunctionLookup) GetAllPodStatuses() map[string]PodStatus {
	return l.podStatusCache.GetAll()
}
