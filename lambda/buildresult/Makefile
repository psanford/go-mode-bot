buildresult: buildresult.go
	go test .
	go build .

.PHONY: upload
upload: buildresult
	zip buildresult.zip buildresult
	aws --region us-west-2 lambda update-function-code --function-name go-mode-bot-build-complete --zip-file fileb://buildresult.zip
	rm buildresult.zip
	rm buildresult
