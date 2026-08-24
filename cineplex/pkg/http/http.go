package http

import (
	"net/http"
	"net/http/cookiejar"
	"time"
)

func NewHttpClientWithCookies(timeout time.Duration) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	return &http.Client{
		Jar:     jar,
		Timeout: timeout,
	}, nil
}
