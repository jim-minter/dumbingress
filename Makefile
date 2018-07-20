dumbingress: clean
	CGO_ENABLED=0 go build .

dumbingress-image: dumbingress
	go get github.com/openshift/imagebuilder/cmd/imagebuilder
	imagebuilder -f Dockerfile -t docker.io/jimminter/dumbingress:latest .

dumbingress-push: dumbingress-image
	docker push docker.io/jimminter/dumbingress:latest

clean:
	rm -f dumbingress

.PHONY: clean dumbingress-image dumbingress-push
