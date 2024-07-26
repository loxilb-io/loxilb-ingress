.DEFAULT_GOAL := build
bin=loxilb-ingress
TAG?=latest

build:
	@mkdir -p ./bin
	go mod tidy
	go build -o ./bin/loxilb-ingress -ldflags="-X 'main.BuildInfo=${shell date '+%Y_%m_%d'}-${shell git branch --show-current}-$(shell git show --pretty=format:%h --no-patch)'" .

clean:
	go clean .

docker: build
	sudo docker build -t ghcr.io/loxilb-io/${bin}:${TAG} .
