package mpesa

import "testing"

func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"0712345678":     "254712345678",
		"+254712345678":  "254712345678",
		"254 712 345678": "254712345678",
	}
	for in, want := range cases {
		got, err := NormalizePhone(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q err %v", in, got, err)
		}
	}
	if _, err := NormalizePhone("12ab"); err == nil {
		t.Error("expected error for invalid number")
	}
}

func TestParseCallbackSuccess(t *testing.T) {
	body := []byte(`{"Body":{"stkCallback":{"MerchantRequestID":"1","CheckoutRequestID":"ws_CO_1","ResultCode":0,"ResultDesc":"ok",
	"CallbackMetadata":{"Item":[{"Name":"Amount","Value":500},{"Name":"MpesaReceiptNumber","Value":"QKX123ABC"},{"Name":"PhoneNumber","Value":254712345678}]}}}}`)
	r, err := ParseCallback(body)
	if err != nil || !r.Success || r.ReceiptNumber != "QKX123ABC" || r.Amount != 500 {
		t.Fatalf("unexpected %+v %v", r, err)
	}
}

func TestParseCallbackCancelled(t *testing.T) {
	body := []byte(`{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_2","ResultCode":1032,"ResultDesc":"Request cancelled by user"}}}`)
	r, err := ParseCallback(body)
	if err != nil || r.Success {
		t.Fatalf("expected failed result, got %+v %v", r, err)
	}
}
