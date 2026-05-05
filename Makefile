.PHONY: test lint fmt install-tools

test:
	go test -race ./...

lint:
	go vet ./...
	staticcheck ./...
	gosec -quiet ./...

fmt:
	goimports -l -w .

install-tools:
	go install golang.org/x/tools/cmd/goimports@v0.44.0
	go install honnef.co/go/tools/cmd/staticcheck@v0.7.0
	go install github.com/securego/gosec/v2/cmd/gosec@v2.26.1
