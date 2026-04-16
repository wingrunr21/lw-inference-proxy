BINARY  := lw-inference-proxy
MODULE  := github.com/wingrunr21/lw-inference-proxy
IMAGE   := $(BINARY):latest

.PHONY: build test lint docker docker-multiarch clean

build:
	go build -o bin/$(BINARY) ./cmd/proxy

test:
	go test ./...

lint:
	golangci-lint run

docker:
	docker build -t $(IMAGE) .

docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE) .

clean:
	rm -rf bin/
