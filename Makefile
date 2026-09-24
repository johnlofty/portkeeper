# local-gateway
#
# `make build` then `make run` is the development loop. `make install` is the
# only target that touches launchd, and it is deliberately never a dependency
# of anything else -- installing a persistent background agent is a decision,
# not a build step.

BINARY  := bin/portkeeperd
PLIST   := io.github.johnlofty.portkeeper.plist
LABEL   := io.github.johnlofty.portkeeper
AGENTS  := $(HOME)/Library/LaunchAgents
REPO    := $(shell pwd)

.PHONY: all build test vet fmt check run install uninstall status logs clean

all: build

build:
	go build -o $(BINARY) ./cmd/portkeeperd

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w ./cmd

# The plist is the one file here no compiler ever reads, so a typo in it only
# shows up as launchd silently refusing to load the agent.
check:
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

clean:
	rm -f $(BINARY)
