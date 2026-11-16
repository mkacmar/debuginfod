.PHONY: test lint fmt

TOOL = go tool -modfile=tools/tools.mod

test:
	go test -v -race ./...

lint:
	go vet ./...
	$(TOOL) staticcheck ./...
	$(TOOL) gosec -quiet ./...

fmt:
	$(TOOL) goimports -l -w .
