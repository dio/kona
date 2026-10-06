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
