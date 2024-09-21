docker:
	docker buildx build --platform=linux/amd64,linux/arm64,linux/386,linux/arm/v7,linux/arm/v6 -t ${DOCKER_TAG} --push .

.PHONY: docker
