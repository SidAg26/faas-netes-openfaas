package handlers

import (
    "encoding/json"
    "net/http"

    "github.com/openfaas/faas-netes/pkg/k8s"
)

func MakePodIdleHandler(lookup *k8s.FunctionLookup) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var req struct {
            PodName string `json:"podName"`
            PodIP   string `json:"podIP"`
        }
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            http.Error(w, "invalid request", http.StatusBadRequest)
            return
        }
        if err := lookup.MarkPodIdle(req.PodName, req.PodIP); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusOK)
    }
}
