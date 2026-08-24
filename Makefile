# SPDX-License-Identifier: AGPL-3.0-or-later
#
# VectoraDB runs inside the Linux dev VM (ZFS + Docker); day-to-day operation is
# via `lima /tmp/vectoradb <command>`. This Makefile just builds/checks the CLI.

.PHONY: build vet fmt vm-build

build:            ## Build the CLI into ./bin/vectoradb (host)
	go build -o bin/vectoradb ./cmd/vectoradb

vet:              ## go vet
	go vet ./...

fmt:              ## list files needing gofmt
	gofmt -l cmd internal

vm-build:         ## Build the Linux binary inside the Lima VM to /tmp/vectoradb
	lima bash -c 'cd "$(CURDIR)" && go build -o /tmp/vectoradb ./cmd/vectoradb'
