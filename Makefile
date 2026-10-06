.PHONY: test images integration chart
test:
	go test -race ./internal/... ./integration ./cmd/bootstrap
images:
	docker build --target proxy -t kona-proxy:spike .
	docker build --target data -t kona-data:spike .
integration: images
	KONA_INTEGRATION=1 go test -v -timeout 25m ./integration

chart:
	helm lint charts/kona
	mkdir -p artifacts
	helm package charts/kona --destination artifacts

.PHONY: native-docker-test
native-docker-test:
	docker build --target native-test -t kona-native-test:spike .
	docker run --rm kona-native-test:spike

ENVOY_BIN ?= $(shell command -v envoy)
.PHONY: native-test native-build
native-build:
	mkdir -p .bin
	CGO_ENABLED=1 go build -modfile=native.mod -trimpath -buildmode=c-shared -o .bin/libkona.so ./cmd/module
native-test: native-build
	@test -n "$(ENVOY_BIN)" || (echo 'Set ENVOY_BIN to the 0a804c57 Envoy binary'; exit 1)
	@"$(ENVOY_BIN)" --version 2>&1 | grep -q 0a804c57 || (echo 'native.mod requires Envoy commit 0a804c57'; exit 1)
	ENVOY_BIN="$(ENVOY_BIN)" KONA_MODULE="$(CURDIR)/.bin/libkona.so" go test -modfile=native.mod -v -count=1 -timeout=60s ./native
