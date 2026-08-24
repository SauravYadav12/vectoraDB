# SPDX-License-Identifier: AGPL-3.0-or-later
#
# VectoraDB runs inside the Linux dev VM (ZFS + Docker); day-to-day operation is
# via `lima /tmp/vectoradb <command>`. This Makefile just builds/checks the CLI.

.PHONY: build vet fmt vm-build test integration web-dev web-build

build:            ## Build the CLI into ./bin/vectoradb (host)
	go build -o bin/vectoradb ./cmd/vectoradb

vet:              ## go vet
	go vet ./...

fmt:              ## list files needing gofmt
	gofmt -l cmd internal

vm-build:         ## Build the Linux binary inside the Lima VM to /tmp/vectoradb
	lima bash -c 'cd "$(CURDIR)" && go build -o /tmp/vectoradb ./cmd/vectoradb'

test:             ## Run unit tests (host, no VM needed)
	go test ./...

integration:      ## Run the full end-to-end integration test in the Lima VM
	lima bash -c 'cd "$(CURDIR)" && go build -o /tmp/vectoradb ./cmd/vectoradb' && lima bash "$(CURDIR)/scripts/integration_test.sh"

web-dev:          ## Run the web UI dev server (http://localhost:5173) against the API
	npm --prefix web install --no-audit --no-fund && npm --prefix web run dev

web-build:        ## Build the web UI to web/dist (static site)
	npm --prefix web ci && npm --prefix web run build
