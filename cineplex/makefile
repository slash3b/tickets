.PHONY:all test test-coverage install-tools fmt vet lint

all: fmt test

test:
	go test -v -race -shuffle=on -timeout=1m -count=1 ./...

test-coverage:
	@go test -race -failfast -shuffle=on -timeout=1m -count=1 -cover -coverprofile=out.html
	@go tool cover -html=out.html

install-tools:
	go install mvdan.cc/gofumpt@v0.8.0
	go install github.com/daixiang0/gci@v0.13.6
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.4.0
	go install github.com/air-verse/air@latest

fmt:
	@gofumpt -w -extra .
	@gci write \
			--custom-order \
			--section standard \
			--section default \
			--section blank \
			--section dot \
			--skip-generated \
			.
vet:
	go vet ./...

lint:
	@golangci-lint run \
		-c .golangci.yml \
		-v \
		./...
