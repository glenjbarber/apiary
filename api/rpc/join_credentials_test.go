package rpcpb

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestJoinCredentialFieldsRoundTrip(t *testing.T) {
	wantRequest := &GetJoinRequestStatusRequest{
		RequestId: "jreq-0123456789abcdef", IntroductionCode: "123456",
		RecipientTlsCertificatePem: "-----BEGIN CERTIFICATE-----\npublic only\n-----END CERTIFICATE-----\n",
	}
	wire, err := proto.Marshal(wantRequest)
	if err != nil {
		t.Fatal(err)
	}
	var gotRequest GetJoinRequestStatusRequest
	if err := proto.Unmarshal(wire, &gotRequest); err != nil {
		t.Fatal(err)
	}
	if gotRequest.GetRecipientTlsCertificatePem() != wantRequest.GetRecipientTlsCertificatePem() {
		t.Fatal("recipient certificate did not round-trip")
	}

	wantResponse := &GetJoinRequestStatusResponse{SealedCredentials: []byte("ciphertext-only")}
	wire, err = proto.Marshal(wantResponse)
	if err != nil {
		t.Fatal(err)
	}
	var gotResponse GetJoinRequestStatusResponse
	if err := proto.Unmarshal(wire, &gotResponse); err != nil {
		t.Fatal(err)
	}
	if string(gotResponse.GetSealedCredentials()) != string(wantResponse.GetSealedCredentials()) {
		t.Fatal("sealed credential envelope did not round-trip")
	}
}
