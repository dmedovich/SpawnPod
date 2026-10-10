.PHONY: test test-short vet check release

test:
	go test ./...
test-short:
	go test -short ./...
vet:
	go vet ./...
check: test vet
	go mod verify
	git diff --check
release:
	./scripts/release.sh
