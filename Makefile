BIN := bin/herdr-tasks

.PHONY: build test link unlink

build:
	go build -o $(BIN) ./cmd/herdr-tasks

test:
	go test ./...

# For working on the plugin: use this checkout instead of a GitHub install.
link: build
	$(BIN) setup
	herdr plugin link $(CURDIR)

unlink:
	$(BIN) remove
