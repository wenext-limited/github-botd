.PHONY: build test race vet check demo clean
build:
	mkdir -p bin
	go build -trimpath -o bin/github-botd ./cmd/github-botd
	go build -trimpath -o bin/github-botctl ./cmd/github-botctl
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
check: test race vet
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))"
demo: build
	sh scripts/demo.sh
clean:
	rm -f bin/github-botd bin/github-botctl coverage.out
