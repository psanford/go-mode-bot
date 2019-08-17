package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/codebuild"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/google/go-github/v27/github"
	"golang.org/x/oauth2"
)

var (
	repoOwner     = "psanford"
	repoName      = "go-mode-hook-test"
	region        = "us-west-2"
	cbProjectName = "go-mode-tests"

	ssmGithubToken = "/prod/lambda/go-mode-bot-build-complete/github-token"

	ghToken string

	cb *codebuild.CodeBuild

	offline = flag.Bool("offline", false, "Run in standalone mode")
)

func init() {
	cb = codebuild.New(session.New(
		&aws.Config{
			Region: &region,
		},
	))
}

func main() {
	flag.Parse()

	if *offline {
		token, err := ioutil.ReadFile("../../token")
		if err != nil {
			panic(err)
		}
		token = bytes.TrimSpace(token)
		ghToken = string(token)
		err = Handler(events.CloudWatchEvent{})
		if err != nil {
			log.Fatal(err)
		}
	} else {
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
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: string(ghToken)},
	)
	tc := oauth2.NewClient(ctx, ts)

	client := github.NewClient(tc)

	notifications, _, err := client.Activity.ListRepositoryNotifications(ctx, repoOwner, repoName, nil)
	if err != nil {
		return fmt.Errorf("List Notifications error: %s", err)
	}
	for _, n := range notifications {
		nName := n.GetRepository().GetFullName()

		if nName != repoOwner+"/"+repoName {
			log.Printf("%s Doesn't match %s/%s, skipping", nName, repoOwner, repoName)
		}

		n.GetRepository().GetOwner()
		s := n.GetSubject()
		log.Printf("%s Notification : %s\n", nName, s.GetType())

		urlParts := strings.Split(s.GetURL(), "/")
		idStr := urlParts[len(urlParts)-1]
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return fmt.Errorf("ID str to int failed %s %s", idStr, err)
		}

		// github_state/#{issue_number}/#{comment_that_triggered_build}

		if s.GetType() == "PullRequest" {
			issue, _, err := client.Issues.Get(ctx, repoOwner, repoName, id)
			if err != nil {
				return fmt.Errorf("Get issue %s/%s #%d err: %s", repoOwner, repoName, id, err)
			}

			var issueNumber int
			if issue.Number != nil {
				issueNumber = *issue.Number
			}

			if issueNumber < 1 {
				log.Printf("Failed to get issue number")
				continue
			}

			issueStr := fmt.Sprintf("%s/%s/pull/%d", repoOwner, repoName, issueNumber)

			if issue.State != nil && *issue.State == StateClosed {
				log.Printf("%s closed, mark notification read", issueStr)
				_, err = client.Activity.MarkThreadRead(ctx, n.GetID())
				if err != nil {
					return fmt.Errorf("MarkThreadRead err: %s", err)
				}
				continue
			}

			var needBuild bool
			var lastBuildRequest int64

			if strings.Index(issue.GetBody(), "@go-mode-bot run") >= 0 && isAuthorized(userName(issue)) {
				needBuild = true
				lastBuildRequest = issue.GetID()
			}

			comments, _, err := client.Issues.ListComments(ctx, repoOwner, repoName, id, nil)
			for _, comment := range comments {
				login := userName(comment)

				if login == "go-mode-bot" {
					needBuild = false
				} else {
					b := comment.GetBody()
					if strings.Index(b, "@go-mode-bot run") >= 0 {
						if isAuthorized(login) {
							needBuild = true
							lastBuildRequest = comment.GetID()
						}
					}
				}
			}

			if needBuild {
				log.Printf("%s needs build for %d", issueStr, lastBuildRequest)

				startReq := &codebuild.StartBuildInput{
					ProjectName: &cbProjectName,
					EnvironmentVariablesOverride: []*codebuild.EnvironmentVariable{
						&codebuild.EnvironmentVariable{
							Name:  aws.String("REPO"),
							Value: aws.String(repoOwner + "/" + repoName),
						},
						&codebuild.EnvironmentVariable{
							Name:  aws.String("PR"),
							Value: aws.String(strconv.Itoa(issueNumber)),
						},
						&codebuild.EnvironmentVariable{
							Name:  aws.String("TRIGGER_COMMENT"),
							Value: aws.String(strconv.Itoa(int(lastBuildRequest))),
						},
					},
				}

				out, err := cb.StartBuild(startReq)
				if err != nil {
					return fmt.Errorf("CB StartBuild err: %s", err)
				}

				var cbID string
				if out.Build != nil && out.Build.Id != nil {
					cbID = *out.Build.Id
				}

				// // trigger build
				body := fmt.Sprintf("Build Prending: %s comment=%d cbID=%s", issueStr, lastBuildRequest, cbID)
				comment := github.IssueComment{
					Body: &body,
				}
				_, _, err = client.Issues.CreateComment(ctx, repoOwner, repoName, id, &comment)
				if err != nil {
					return fmt.Errorf("Update Comment err: %s", err)
				}
			} else {
				log.Printf("%s No build needed", issueStr)
			}

			log.Printf("%s Mark thread read", issueStr)
			_, err = client.Activity.MarkThreadRead(ctx, n.GetID())
			if err != nil {
				return fmt.Errorf("MarkThreadRead err: %s", err)
			}
		}
	}
	return nil
}

const StateClosed = "closed"

func isAuthorized(u string) bool {
	return u == "psanford" || u == "muirrn" || u == "muirmanders" || u == "dominikh"
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
