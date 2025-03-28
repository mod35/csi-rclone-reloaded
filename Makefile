# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

VERSION=$(shell cat VERSION)
REGISTRY_NAME=modes
IMAGE_NAME=csi-rclone-reloaded
IMAGE_TAG=$(REGISTRY_NAME)/$(IMAGE_NAME):$(VERSION)

.PHONY: all rclone-plugin clean rclone-container

all: plugin container push
dm: plugin-dm container-dm push-dm

plugin:
	go mod download
	CGO_ENABLED=0 GOOS=linux go build -a -gcflags=-trimpath=$(go env GOPATH) -asmflags=-trimpath=$(go env GOPATH) -ldflags '-X github.com/wunderio/csi-rclone/pkg/rclone.DriverVersion=$(VERSION) -extldflags "-static"' -o _output/csi-rclone-plugin ./cmd/csi-rclone-plugin
	
plugin-dm:
	go mod download
	CGO_ENABLED=0 GOOS=linux go build -a -gcflags=-trimpath=$(go env GOPATH) -asmflags=-trimpath=$(go env GOPATH) -ldflags '-X github.com/wunderio/csi-rclone/pkg/rclone.DriverVersion=$(VERSION)-dm -extldflags "-static"' -o _output/csi-rclone-plugin-dm ./cmd/csi-rclone-plugin
	
container:
	docker build -t $(IMAGE_TAG) -f ./cmd/csi-rclone-plugin/Dockerfile .

container-dm:
	docker build -t $(IMAGE_TAG)-dm -f ./cmd/csi-rclone-plugin/Dockerfile.dm .

push:
	docker push $(IMAGE_TAG)

push-dm:
	docker push $(IMAGE_TAG)-dm

buildx:
	docker buildx build --platform linux/amd64,linux/arm64 --push -t $(IMAGE_TAG) -f ./cmd/csi-rclone-plugin/Dockerfile .

buildx-dm:
	docker buildx build --platform linux/amd64,linux/arm64 --push -t $(IMAGE_TAG)-dm -f ./cmd/csi-rclone-plugin/Dockerfile.dm .


clean:
	go clean -r -x
	-rm -rf _output
