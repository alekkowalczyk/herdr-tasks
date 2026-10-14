BIN := bin/herdr-tasks

.PHONY: build test install link unlink

build:
	go build -o $(BIN) ./cmd/herdr-tasks

test:
	go test ./...

# Puts herdr-tasks on your PATH, for herdr's tab bar status and for agents.
install: build
	mkdir -p $(HOME)/.local/bin
	ln -sf $(CURDIR)/$(BIN) $(HOME)/.local/bin/herdr-tasks

# Registers this checkout with herdr. Changes take effect on the next build.
link: install
	herdr plugin link $(CURDIR)
	$(BIN) sync

unlink:
	herdr plugin unlink herdr-tasks
