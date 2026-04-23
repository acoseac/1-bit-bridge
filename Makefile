.PHONY: build build-all test fmt vet clean run

BINARY      := bridge
PKG         := ./cmd/bridge
DIST        := dist
LDFLAGS     := -s -w

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

build-all:
	@mkdir -p $(DIST)
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-arm64  $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-darwin-amd64  $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-arm64   $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64   $(PKG)
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)

test:
	go test -race ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf bin $(DIST)

run:
	go run $(PKG) serve --config config/bridge.yaml.example
