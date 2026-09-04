package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestRetryAcrossCredentialPolicy(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		generic int
		want    bool
	}{
		{name: "timeout", err: &Error{HTTPStatus: http.StatusGatewayTimeout}, want: true},
		{name: "temporary 500", err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "Request timed out"}, want: true},
		{name: "generic first 500", err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "unexpected"}, want: true},
		{name: "generic second 500", err: &Error{HTTPStatus: http.StatusInternalServerError, Message: "unexpected"}, generic: 1, want: false},
		{name: "bad request", err: &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid request"}, want: false},
		{name: "network deadline", err: context.DeadlineExceeded, want: true},
		{name: "unknown error", err: errors.New("local failure"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := retryAcrossCredential(tt.err, tt.generic)
			if got != tt.want {
				t.Fatalf("retry = %v, want %v", got, tt.want)
			}
		})
	}
}
