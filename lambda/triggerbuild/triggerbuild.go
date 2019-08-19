package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
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
	repos = []repo{
		{
			owner: "dominikh",
			name:  "go-mode.el",
		},
		{
			owner: "psanford",
			name:  "go-mode-hook-test",
		},
	}

	region        = "us-west-2"
	cbProjectName = "go-mode-tests"

	ssmGithubToken = "/prod/lambda/go-mode-bot-build-complete/github-token"

	ghToken string
	cb      *codebuild.CodeBuild

	offline = flag.Bool("offline", false, "Run in standalone mode")
)

const (
	eyesReaction = "eyes"
	botID        = "go-mode-bot"
)

type repo struct {
	owner string
	name  string
}

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

	for _, repo := range repos {
		notifications, _, err := client.Activity.ListRepositoryNotifications(ctx, repo.owner, repo.name, nil)
		if err != nil {
			return fmt.Errorf("List Notifications error: %s", err)
		}
		for _, n := range notifications {
			nName := n.GetRepository().GetFullName()

			if nName != repo.owner+"/"+repo.name {
				log.Printf("%s Doesn't match %s/%s, skipping", nName, repo.owner, repo.name)
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
				issue, _, err := client.Issues.Get(ctx, repo.owner, repo.name, id)
				if err != nil {
					return fmt.Errorf("Get issue %s/%s #%d err: %s", repo.owner, repo.name, id, err)
				}

				var issueNumber int
				if issue.Number != nil {
					issueNumber = *issue.Number
				}

				if issueNumber < 1 {
					log.Printf("Failed to get issue number")
					continue
				}

				issueStr := fmt.Sprintf("%s/%s/pull/%d", repo.owner, repo.name, issueNumber)

				if issue.State != nil && *issue.State == StateClosed {
					log.Printf("%s closed, mark notification read", issueStr)
					_, err = client.Activity.MarkThreadRead(ctx, n.GetID())
					if err != nil {
						return fmt.Errorf("MarkThreadRead err: %s", err)
					}
					continue
				}

				var (
					needBuild        bool
					lastBuildRequest int64
				)

				if strings.Index(issue.GetBody(), "@go-mode-bot run") >= 0 && isAuthorized(userName(issue)) {

					reactions, _, err := client.Reactions.ListIssueReactions(ctx, repo.owner, repo.name, id, nil)
					if err != nil {
						return fmt.Errorf("ListCommentReactions err: %s", err)
					}
					var hasBotReaction bool
					for _, r := range reactions {
						if userName(r) == botID && r.GetContent() == eyesReaction {
							hasBotReaction = true
							break
						}
					}

					if !hasBotReaction {
						needBuild = true
						lastBuildRequest = issue.GetID()
						_, _, err := client.Reactions.CreateIssueReaction(ctx, repo.owner, repo.name, id, eyesReaction)
						if err != nil {
							return fmt.Errorf("CreateIssueReaction err: %s", err)
						}
					}
				}

				comments, _, err := client.Issues.ListComments(ctx, repo.owner, repo.name, id, nil)
				for _, comment := range comments {
					login := userName(comment)

					if login == botID {
						continue
					} else {
						b := comment.GetBody()
						if strings.Index(b, "@go-mode-bot run") >= 0 {
							if isAuthorized(login) {

								reactions, _, err := client.Reactions.ListIssueCommentReactions(ctx, repo.owner, repo.name, comment.GetID(), nil)

								if err != nil {
									return fmt.Errorf("ListCommentReactions err: %s", err)
								}
								var hasBotReaction bool
								for _, r := range reactions {
									if userName(r) == botID && r.GetContent() == eyesReaction {
										hasBotReaction = true
										break
									}
								}

								if !hasBotReaction {
									needBuild = true
									lastBuildRequest = comment.GetID()
									client.Reactions.CreateIssueCommentReaction(ctx, repo.owner, repo.name, comment.GetID(), eyesReaction)
									if err != nil {
										return fmt.Errorf("CreateIssueReaction err: %s", err)
									}
								}
							}
						}
					}
				}

				if needBuild {
					log.Printf("%s needs build for %d", issueStr, lastBuildRequest)

					botID := randID()

					startReq := &codebuild.StartBuildInput{
						ProjectName: &cbProjectName,
						EnvironmentVariablesOverride: []*codebuild.EnvironmentVariable{
							&codebuild.EnvironmentVariable{
								Name:  aws.String("REPO"),
								Value: aws.String(repo.owner + "/" + repo.name),
							},
							&codebuild.EnvironmentVariable{
								Name:  aws.String("PR"),
								Value: aws.String(strconv.Itoa(issueNumber)),
							},
							&codebuild.EnvironmentVariable{
								Name:  aws.String("GO_MODE_BOT_ID"),
								Value: aws.String(botID),
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
					log.Printf("Triggered build commentID=%d cbID=%s gmbID=%s", lastBuildRequest, cbID, botID)
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
	}
	return nil
}

const StateClosed = "closed"

type pendingReaction struct {
	repo       string
	owner      string
	id         int64
	createFunc func(ctx context.Context, owner string, repo string, id int64, content string) (*github.Reaction, *github.Response, error)
}

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

func randID() string {
	b := make([]byte, 16)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%X\n", b)
}
