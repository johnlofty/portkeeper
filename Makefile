# portkeeper
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

# The tracked plist carries __REPO__ where this checkout's path belongs, so no home
# directory is ever committed. install renders it into place instead of symlinking,
# and unloads the previous rendering first so launchd drops the old definition.
install: build
	@mkdir -p $(AGENTS)
	-launchctl unload $(AGENTS)/$(PLIST) 2>/dev/null
	sed 's#__REPO__#$(REPO)#g' $(PLIST) > $(AGENTS)/$(PLIST)
	launchctl load $(AGENTS)/$(PLIST)
	@echo "loaded: $(LABEL) -- logs at /tmp/portkeeper.log"

uninstall:
	-launchctl unload $(AGENTS)/$(PLIST) 2>/dev/null
	rm -f $(AGENTS)/$(PLIST)
	@echo "unloaded: $(LABEL)"

status:
	@launchctl list | grep $(LABEL) || echo "$(LABEL) is not loaded"

logs:
	@tail -f /tmp/portkeeper.log

clean:
	rm -f $(BINARY)

# ---- the menu-bar app -------------------------------------------------------------
#
# `make app` builds build/Portkeeper.app: the Swift menu-bar client with the daemon
# bundled beside it. Like `build`, it touches nothing outside the repo.
#
# `make app-install` copies the bundle to ~/Applications and unloads the dev agent
# (and removes its plist, so it does not come back at the next login),
# because the bundle's own helper and the dev agent would both bind 9996 and only one
# can. It does not register the helper: that is done from the app's Settings, from the
# installed copy, because SMAppService records the bundle's location at registration.

APP_BUNDLE   := build/Portkeeper.app
APP_DEST     := $(HOME)/Applications/Portkeeper.app

.PHONY: app app-install dist

app:
	macos/build-app.sh

app-install: app
	-launchctl unload $(AGENTS)/$(PLIST) 2>/dev/null
	rm -f $(AGENTS)/$(PLIST)
	@mkdir -p $(HOME)/Applications
	rm -rf $(APP_DEST)
	cp -R $(APP_BUNDLE) $(APP_DEST)
	@echo "installed: $(APP_DEST)"
	@echo "unloaded the dev agent $(LABEL), if it was loaded."
	@echo "next: open $(APP_DEST), then Settings > Background helper > Register"
	@echo "      (and approve it in System Settings > Login Items if asked)."

# A downloadable zip of the app. `ditto` keeps the bundle's signature and resource
# metadata intact where `zip -r` would not. VERSION is passed in (a tag in CI); it never
# comes from `git describe`, which has nothing to describe on a shallow checkout.
VERSION ?= dev
dist: app
	@mkdir -p dist
	rm -f dist/Portkeeper-$(VERSION)-macos-arm64.zip
	ditto -c -k --sequesterRsrc --keepParent build/Portkeeper.app dist/Portkeeper-$(VERSION)-macos-arm64.zip
	@ls -la dist/Portkeeper-$(VERSION)-macos-arm64.zip
