BIN := bin/niwakubed

.PHONY: build test vet install

build:
	CGO_ENABLED=0 go build -o $(BIN) ./cmd/niwakubed

test:
	go test -race ./...

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

install: build
	install -m 0755 $(BIN) /usr/local/bin/niwakubed
	install -m 0644 deploy/niwakubed.service /etc/systemd/system/niwakubed.service
	systemctl daemon-reload
