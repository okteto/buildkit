DESTDIR=/usr/local

binaries: FORCE
	hack/binaries

images: FORCE
# moby/buildkit:local and moby/buildkit:local-rootless are created on Docker
	hack/images local moby/buildkit

install: FORCE
	mkdir -p $(DESTDIR)/bin
	install bin/* $(DESTDIR)/bin

clean: FORCE
	rm -rf ./bin

test:
	./hack/test integration gateway dockerfile

lint:
	./hack/lint

validate-vendor:
	./hack/validate-vendor

validate-generated-files:
	./hack/validate-generated-files

validate-all: test lint validate-vendor validate-generated-files

vendor:
	./hack/update-vendor

generated-files:
	./hack/update-generated-files

okteto-push:
	docker push okteto/buildkit:v0.7.2-1
	docker push okteto/buildkit:v0.7.2-rootless1

okteto-tag:
	docker tag moby/buildkit:local okteto/buildkit:v0.7.2-1
	docker tag moby/buildkit:local-rootless okteto/buildkit:v0.7.2-rootless1

okteto: images okteto-tag okteto-push

.PHONY: vendor generated-files test binaries images install clean lint validate-all validate-vendor validate-generated-files okteto okteto-push okteto-tag
FORCE:
