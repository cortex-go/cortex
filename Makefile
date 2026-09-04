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
	node --check public/assets/js/script.js
	node tests/markdown-render-smoke.js
	node tests/session-navigation.test.js
