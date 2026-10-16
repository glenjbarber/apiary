package rpcpb_test

import (
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestWatchgooseNodeConfigRPCFieldsRoundTrip(t *testing.T) {
	enabled := false
	request := &rpcpb.UpdateNodeConfigRequest{
		WatchgooseEnabled: &enabled, WatchgooseEndpoint: "https://watchgoose.com/ping/slug/hive",
		ClearWatchgooseEndpoint: false, WatchgooseInterval: "5m", WatchgooseTimeout: "10s",
		WatchgooseBadgeUrl: "https://watchgoose.com/badge/check/badge-token.svg",
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded rpcpb.UpdateNodeConfigRequest
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WatchgooseEnabled == nil || decoded.GetWatchgooseEnabled() || decoded.GetWatchgooseEndpoint() != request.GetWatchgooseEndpoint() || decoded.GetWatchgooseInterval() != "5m" || decoded.GetWatchgooseTimeout() != "10s" || decoded.GetWatchgooseBadgeUrl() != request.GetWatchgooseBadgeUrl() {
		t.Fatalf("decoded request = %+v", &decoded)
	}
	decoded.ClearWatchgooseBadgeUrl = true
	if !decoded.GetClearWatchgooseBadgeUrl() {
		t.Fatal("clear_watchgoose_badge_url did not round trip")
	}
	json, err := protojson.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var response rpcpb.GetNodeConfigResponse
	if err := protojson.Unmarshal([]byte(`{"watchgooseEnabled":true,"watchgooseEndpointSet":true,"watchgooseInterval":"5m","watchgooseTimeout":"10s","watchgooseBadgeUrl":"https://watchgoose.com/badge/check/badge-token.svg"}`), &response); err != nil {
		t.Fatal(err)
	}
	if !response.GetWatchgooseEnabled() || !response.GetWatchgooseEndpointSet() || response.GetWatchgooseInterval() != "5m" || response.GetWatchgooseTimeout() != "10s" || response.GetWatchgooseBadgeUrl() == "" {
		t.Fatalf("decoded response = %+v", &response)
	}
	if len(json) == 0 {
		t.Fatal("protojson returned an empty request")
	}
}
