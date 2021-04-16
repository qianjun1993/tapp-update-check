.PHONY: all
all: verify-gofmt build

.PHONY: clean
clean:
	rm -rf bin/ _output/ go .version-defs

.PHONY: build
build:
	hack/build.sh


.PHONY: verify-gofmt
verify-gofmt:
	hack/verify-gofmt.sh