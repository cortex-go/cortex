.PHONY: frontend test build check

frontend:
	nift build

build: frontend
	go build -o cortex ./cmd/cortex

test:
	go test ./...

check: frontend
	nift status
	go test ./...
	go vet ./...
	node --check content/assets/js/script.js
