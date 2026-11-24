.PHONY: run test lint cover

run:
	go run ./cmd/server

# -race catches data races; -count=1 disables the test cache so results are fresh.
test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

cover:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1