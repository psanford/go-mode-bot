triggerbuild: triggerbuild.go
	go test .
	go build .

.PHONY: upload
upload: triggerbuild
	zip triggerbuild.zip triggerbuild
	aws --region us-west-2 lambda update-function-code --function-name go-mode-bot-trigger-build --zip-file fileb://triggerbuild.zip
	rm triggerbuild.zip
	rm triggerbuild
