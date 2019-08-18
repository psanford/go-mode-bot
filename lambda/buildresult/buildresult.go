package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

const (
	StatusSuccess = "SUCCEEDED"
)

var (
	offline = flag.String("offline", "", "Path to event.json")

	ssmGithubToken = "/prod/lambda/go-mode-bot-build-complete/github-token"

	ghToken string
)

func main() {
	flag.Parse()

	if *offline != "" {
		body, err := ioutil.ReadFile(*offline)
		if err != nil {
			panic(err)
		}

		t, err := ioutil.ReadFile("../token")
		if err != nil {
			panic(err)
		}
		t = bytes.TrimSpace(t)
		ghToken = string(t)

		e := events.CloudWatchEvent{
			Detail: json.RawMessage(body),
		}
		err = Handler(e)
		if err != nil {
			panic(err)
		}
	} else {
		var region = "us-west-2"
		awsSession := session.New(&aws.Config{
			Region: &region,
		})
		ssmClient := ssm.New(awsSession)

		req := ssm.GetParameterInput{
			Name:           &ssmGithubToken,
			WithDecryption: aws.Bool(true),
		}
		resp, err := ssmClient.GetParameter(&req)
		if err != nil {
			log.Fatalf("Failed to get ssm parameter %s: %s", ssmGithubToken, err)
		}

		val := resp.Parameter.Value
		if val == nil {
			log.Fatalf("Got nil ssm parameter %s", ssmGithubToken)
		}
		ghToken = *val

		lambda.Start(Handler)
	}
}

func Handler(e events.CloudWatchEvent) error {
	ctx := context.Background()
	var b BuildInfo
	err := json.Unmarshal(e.Detail, &b)
	if err != nil {
		return err
	}

	log.Printf("Build result: id=%s status=%s", b.BuildId, b.BuildStatus)

	if b.BuildStatus != StatusSuccess {
		return fmt.Errorf("Build %s status %s", b.BuildId, b.BuildStatus)
	}

	bidParts := strings.Split(b.BuildId, "/")
	if len(bidParts) != 2 {
		return fmt.Errorf("Unexpected build id format %s len == %d", b.BuildId, len(bidParts))
	}

	arnPrefix := bidParts[0]
	cbID := bidParts[1]

	cbIDParts := strings.Split(cbID, ":")
	if len(cbIDParts) != 2 {
		return fmt.Errorf("Unexpected cb id format %s", cbID)
	}
	buildUUID := cbIDParts[1]

	arnParts := strings.Split(arnPrefix, ":")
	if len(arnParts) != 6 || arnParts[0] != "arn" || arnParts[1] != "aws" || arnParts[2] != "codebuild" {
		return fmt.Errorf("Unexpected arn prefix format %s len == %d", arnPrefix, len(arnParts))
	}

	region := arnParts[3]

	locParts := strings.Split(b.AdditionalInformation.Artifact.Location, ":")
	if len(locParts) != 6 || locParts[0] != "arn" || locParts[1] != "aws" || locParts[2] != "s3" {
		return fmt.Errorf("Unexpected location format %s len == %d", b.AdditionalInformation.Artifact.Location, len(locParts))
	}

	bucketPath := strings.SplitN(locParts[5], "/", 2)
	if len(bucketPath) != 2 {
		return fmt.Errorf("Unexpected bucket location format %s", b.AdditionalInformation.Artifact.Location)
	}
	bucket := bucketPath[0]
	s3Prefix := bucketPath[1]

	s3Client := newS3Client(region, bucket, s3Prefix)

	s3URL := func(file string) string {
		p := path.Join(s3Prefix, "artifacts", file)
		return fmt.Sprintf("https://%s.s3.amazonaws.com/%s", bucket, p)
	}

	testStatus := "FAIL"
	i, err := s3Client.getObjInt("emacs-tests.exitcode")
	if err != nil {
		panic(err)
	}
	if i == 0 {
		testStatus = "Pass"
	}

	td, err := s3Client.getObjInt("emacs-tests.runtime")
	if err != nil {
		panic(err)
	}
	testDuration := time.Duration(time.Duration(td) * time.Millisecond)

	reindentStatus := "ERROR"
	i, err = s3Client.getObjInt("batch-reindent.exitcode")
	if err != nil {
		panic(err)
	}
	if i == 0 {
		reindentStatus = "Ok"
	}

	rd, err := s3Client.getObjInt("batch-reindent.runtime")
	if err != nil {
		panic(err)
	}
	reindentDuration := time.Duration(time.Duration(rd) * time.Millisecond)

	diffStat, err := s3Client.getObjStr("batch-reindent.diffstat")
	if err != nil {
		panic(err)
	}

	sha, err := s3Client.getObjStr("git_sha")
	if err != nil {
		panic(err)
	}

	files := []string{"batch-reindent.diff", "batch-reindent.log", "emacs-tests.log"}
	for _, f := range files {
		if err = s3Client.addContentType(f, "text/plain"); err != nil {
			return fmt.Errorf("Add content type  %s failed: %s", f, err)
		}
	}

	diffURL := s3URL("batch-reindent.diff")
	diffOutputURL := s3URL("batch-reindent.log")
	testOutputURL := s3URL("emacs-tests.log")

	var sb strings.Builder
	fmt.Fprintf(&sb, "Build result for %s (build_id=%s)\n", strings.TrimSpace(sha), buildUUID)
	fmt.Fprintf(&sb, "ERT tests %s in %s\n", testStatus, testDuration)
	fmt.Fprintf(&sb, "Test output: [%s](%s)\n\n", path.Base(testOutputURL), testOutputURL)
	fmt.Fprintf(&sb, "Reindent: %s in %s\n", reindentStatus, reindentDuration)
	fmt.Fprintf(&sb, "\n```\n%s\n```\n", diffStat)
	fmt.Fprintf(&sb, "Diff output: [%s](%s)\n", path.Base(diffURL), diffURL)
	fmt.Fprintf(&sb, "Reindent emacs output: [%s](%s)\n", path.Base(diffOutputURL), diffOutputURL)

	log.Printf("Result %s", sb.String())

	var (
		pr             int
		repoOwner      string
		repoName       string
		triggerComment int
		goModeBotID    string
	)

	for _, ev := range b.AdditionalInformation.Environment.EnvironmentVariables {
		switch ev.Name {
		case "PR":
			i, err := strconv.Atoi(ev.Value)
			if err != nil {
				return fmt.Errorf("Failed to parse PR %v\n", ev)
			}
			pr = i
		case "REPO":
			parts := strings.Split(ev.Value, "/")
			if len(parts) != 2 {
				return fmt.Errorf("REPO not in expected owner/name format: %s", ev.Value)
			}
			repoOwner = parts[0]
			repoName = parts[1]
		case "GO_MODE_BOT_ID":
			goModeBotID = ev.Value
		case "TRIGGER_COMMENT":
			i, err := strconv.Atoi(ev.Value)
			if err != nil {
				return fmt.Errorf("Failed to parse TRIGGER_COMMENT %v\n", ev)
			}
			triggerComment = i
		}
	}

	if pr < 1 || repoOwner == "" || repoName == "" {
		return fmt.Errorf("Failed to find all env variables: %+v", b.AdditionalInformation.Environment.EnvironmentVariables)
	}

	_ = triggerComment

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: string(ghToken)},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	idSearch := "gmbID=" + goModeBotID

	var origCommentFound bool

	log.Printf("Search for original pr: %s/%s %d", repoOwner, repoName, pr)

	comments, _, err := client.Issues.ListComments(ctx, repoOwner, repoName, pr, nil)
	if err != nil {
		return fmt.Errorf("ListComments error: %s\n", err)
	}
	log.Printf("Found %d comments", len(comments))
	for _, comment := range comments {
		login := userName(comment)

		if login == "go-mode-bot" {
			b := comment.GetBody()
			if strings.Index(b, idSearch) >= 0 {
				origCommentFound = true
				b := sb.String()
				comment.Body = &b

				_, _, err := client.Issues.EditComment(ctx, repoOwner, repoName, comment.GetID(), comment)
				if err != nil {
					return fmt.Errorf("Error updating comment: %s", err)
				}

				break
			}
		}
	}

	if origCommentFound != true {
		return fmt.Errorf("Failed to find original gobot comment")
	}

	return nil
}

type Owned interface {
	GetUser() *github.User
}

func userName(o Owned) string {
	u := o.GetUser()
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

type s3Client struct {
	bucket string
	prefix string

	client *s3.S3
}

func newS3Client(region, bucket, prefix string) *s3Client {
	awsSession := session.New(&aws.Config{
		Region: &region,
	})
	c := s3.New(awsSession)

	return &s3Client{
		bucket: bucket,
		prefix: prefix,
		client: c,
	}
}

func (c *s3Client) getObjStr(file string) (string, error) {
	p := path.Join(c.prefix, "artifacts", file)
	getObj := &s3.GetObjectInput{
		Bucket: &c.bucket,
		Key:    &p,
	}
	obj, err := c.client.GetObject(getObj)
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(obj.Body)
	if err != nil {
		return "", err
	}
	body = bytes.TrimSpace(body)
	return string(body), nil
}

func (c *s3Client) getObjInt(file string) (int, error) {
	body, err := c.getObjStr(file)
	if err != nil {
		return -128, err
	}
	return strconv.Atoi(body)
}

func (c *s3Client) addContentType(file, contentType string) error {
	p := path.Join(c.prefix, "artifacts", file)
	input := s3.CopyObjectInput{
		Bucket:            &c.bucket,
		Key:               &p,
		CopySource:        aws.String(path.Join(c.bucket, p)),
		ContentType:       &contentType,
		MetadataDirective: aws.String("REPLACE"),
	}
	_, err := c.client.CopyObject(&input)
	return err
}

type BuildInfo struct {
	BuildId               string `json:"build-id"`
	BuildStatus           string `json:"build-status"`
	CurrentPhase          string `json:"current-phase"`
	CurrentPhaseContext   string `json:"current-phase-context"`
	ProjectName           string `json:"project-name"`
	Version               string `json:"version"`
	AdditionalInformation struct {
		Artifact struct {
			Location string `json:"location"`
		} `json:"artifact"`
		BuildComplete  bool   `json:"build-complete"`
		BuildStartTime string `json:"build-start-time"`
		Cache          struct {
			Type string `json:"type"`
		} `json:"cache"`
		Environment struct {
			ComputeType          string `json:"compute-type"`
			EnvironmentVariables []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"environment-variables"`
			Image                    string `json:"image"`
			ImagePullCredentialsType string `json:"image-pull-credentials-type"`
			PrivilegedMode           bool   `json:"privileged-mode"`
			Type                     string `json:"type"`
		} `json:"environment"`
		Initiator string `json:"initiator"`
		Logs      struct {
			DeepLink   string `json:"deep-link"`
			GroupName  string `json:"group-name"`
			StreamName string `json:"stream-name"`
		} `json:"logs"`
		Phases []struct {
			DurationInSeconds float64  `json:"duration-in-seconds"`
			EndTime           string   `json:"end-time"`
			PhaseContext      []string `json:"phase-context"`
			PhaseStatus       string   `json:"phase-status"`
			PhaseType         string   `json:"phase-type"`
			StartTime         string   `json:"start-time"`
		} `json:"phases"`
		QueuedTimeoutInMinutes float64 `json:"queued-timeout-in-minutes"`
		Source                 struct {
			Buildspec string `json:"buildspec"`
			Type      string `json:"type"`
		} `json:"source"`
		TimeoutInMinutes float64 `json:"timeout-in-minutes"`
	} `json:"additional-information"`
}
