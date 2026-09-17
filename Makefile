# local-gateway
#
# `make build` then `make run` is the development loop. `make install` is the
# only target that touches launchd, and it is deliberately never a dependency
# of anything else -- installing a persistent background agent is a decision,
# not a build step.

BINARY  := bin/gatewayd
CLIENT  := bin/expose
PLIST   := io.github.johnlofty.portkeeper.plist
LABEL   := io.github.johnlofty.portkeeper
AGENTS  := $(HOME)/Library/LaunchAgents
REPO    := $(shell pwd)

# The host the client is deployed to, and where it lands on that host.
HOST       := code
CLIENT_DST := ~/.local/bin/expose

.PHONY: all build test vet fmt check run install uninstall status logs \
        deploy-client clean

all: build

build:
	go build -o $(BINARY) ./cmd/gatewayd

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w ./cmd

# The client ships as a script, so its "compile" is a syntax check. /bin/sh on
# the remote is dash, so check with dash when it is available rather than
# letting a bashism through on a bash-is-sh machine.
check:
	@sh -n $(CLIENT) && echo "ok: $(CLIENT) parses under sh"
	@command -v dash >/dev/null 2>&1 \
		&& dash -n $(CLIENT) && echo "ok: $(CLIENT) parses under dash" \
		|| echo "note: dash not installed, skipped the stricter check"
	@plutil -lint $(PLIST)

run: build
	$(BINARY)

install: build
	@mkdir -p $(AGENTS)
	ln -sf $(REPO)/$(PLIST) $(AGENTS)/$(PLIST)
	-launchctl unload $(AGENTS)/$(PLIST) 2>/dev/null
	launchctl load $(AGENTS)/$(PLIST)
	@echo "loaded: $(LABEL) -- logs at /tmp/local-gateway.log"

uninstall:
	-launchctl unload $(AGENTS)/$(PLIST) 2>/dev/null
	rm -f $(AGENTS)/$(PLIST)
	@echo "unloaded: $(LABEL)"

status:
	@launchctl list | grep $(LABEL) || echo "$(LABEL) is not loaded"

logs:
	@tail -f /tmp/local-gateway.log

# Standalone repo for now, so this is a plain scp. Once local-gateway folds
# into the dotfiles repo, the client rides along with the existing clone on
# $(HOST) and install.sh symlinks it into ~/.local/bin -- at which point this
# target becomes `ssh $(HOST) 'cd ~/Project/Github/dotfiles && git pull'`.
deploy-client: check
	scp $(CLIENT) $(HOST):$(CLIENT_DST)
	ssh $(HOST) 'chmod +x $(CLIENT_DST)'
	@echo "deployed $(CLIENT) to $(HOST):$(CLIENT_DST)"

# Removes only the built binary: bin/ also holds the hand-written client.
clean:
	rm -f $(BINARY)
