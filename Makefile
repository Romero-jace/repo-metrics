# The parent workspace directory carries a go.work that does not list this
# module, so every toolchain invocation here has to opt out of it. Without this
# line the toolchain fails with:
#
#   pattern ./...: directory prefix . does not contain modules listed in
#   go.work or their selected dependencies
#
# This module is meant to build standalone (it is going to be transferred out),
# so opting out is the right fix rather than joining the workspace.
export GOWORK=off

BINARY := repo-metrics

# Every target below has a real rule. A .PHONY target with no rule prints
# "Nothing to be done" and exits 0, which is exactly the silent-success failure
# this tool exists to catch, so it would be a poor look to ship one.
.PHONY: build test vet lint fmt tidy check clean

build:
	go build -o ./bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

# check is the pre-commit gate, in the order that fails cheapest first.
check: build vet test lint

clean:
	rm -rf ./bin
