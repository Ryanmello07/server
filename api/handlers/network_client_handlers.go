package handlers

import (
	"net/http"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// AuthNetworkClient provisions a client, and is where an agent meets the plan's
// concurrent-client limit. It carries the full x402 round trip.
func AuthNetworkClient(w http.ResponseWriter, r *http.Request) {
	if !controller.X402SettleInlineUpgrade(w, r) {
		return
	}
	router.WrapWithInputRequireAuth(
		model.AuthNetworkClient,
		w,
		r,
		func(result *model.AuthNetworkClientResult) bool {
			return controller.WriteX402UpgradeRequired(w, result)
		},
	)
}

func RemoveNetworkClient(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(model.RemoveNetworkClient, w, r)
}

func RemoveNetworkClients(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20) // 100 MB cap (allows ~2M UUIDs worst case)
	router.WrapWithInputRequireAuth(model.RemoveNetworkClients, w, r)
}

func RemoveNetwork(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(controller.NetworkRemove, w, r)
}

func NetworkClients(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkClients, w, r)
}

func NetworkPeers(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(model.GetNetworkPeersForSession, w, r)
}
