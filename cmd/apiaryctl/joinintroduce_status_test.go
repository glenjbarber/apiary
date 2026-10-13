package main

import "testing"

// The poll must not name a target_address. join-introduce dials the target
// directly, and a managerd that receives a target_address forwards the
// call onward carrying only the request_id, which drops the first code and
// makes the target withhold the second PIN with no error.
func TestBuildJoinStatusRequest_NamesNoTargetAndCarriesTheFirstCode(t *testing.T) {
	got := buildJoinStatusRequest("jreq-0123456789abcdef", "123456")

	if got.GetTargetAddress() != "" {
		t.Errorf("TargetAddress = %q, want empty: naming one makes the target forward the poll without the first code", got.GetTargetAddress())
	}
	if got.GetRequestId() != "jreq-0123456789abcdef" {
		t.Errorf("RequestId = %q, want the request id", got.GetRequestId())
	}
	if got.GetIntroductionCode() != "123456" {
		t.Errorf("IntroductionCode = %q, want the first code: without it the target cannot release the second PIN", got.GetIntroductionCode())
	}
}
