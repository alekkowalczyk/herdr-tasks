BIN := bin/herdr-tasks

.PHONY: build test link unlink release

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

# Tags the version in herdr-plugin.toml; GitHub then builds the release binaries.
release:
	@v=$$(sed -n 's/^version = "\(.*\)"$$/\1/p' herdr-plugin.toml); \
	grep -q "const version = \"$$v\"" cmd/herdr-tasks/main.go || { echo "main.go version doesn't match $$v"; exit 1; }; \
	git tag "v$$v" && git push origin "v$$v"
