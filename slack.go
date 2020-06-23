package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/shomali11/slacker"
	"github.com/slack-go/slack"
	"k8s.io/client-go/pkg/version"
	"k8s.io/klog"
)

type Bot struct {
	token string
}

func NewBot(token string) *Bot {
	return &Bot{
		token: token,
	}
}

func (b *Bot) Start(manager JobManager) error {
	slack := slacker.NewClient(b.token)

	manager.SetNotifier(b.jobResponder(slack))

	slack.DefaultCommand(func(request slacker.Request, response slacker.ResponseWriter) {
		response.Reply("unrecognized command, msg me `help` for a list of all commands")
	})

	validBundles, err := manager.ListBundles()
	if err != nil {
		return err
	}
	slack.Command("launch <bundle>", &slacker.CommandDefinition{
		Description: fmt.Sprintf(
			"Launch a single node OpenShift cluster using CodeReady Containers from the specified bundle. Valid bundles are %s.", strings.Join(validBundles, ", ")),
		Example: fmt.Sprintf("launch %s", validBundles[0]),
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			user := request.Event().User
			channel := request.Event().Channel
			if !isDirectMessage(channel) {
				response.Reply("this command is only accepted via direct message")
				return
			}

			bundle, err := parseBundleName(request.StringParam("bundle", ""))
			if err != nil {
				response.Reply(err.Error())
				return
			}

			msg, err := manager.LaunchClusterForUser(&ClusterRequest{
				OriginalMessage: stripLinks(request.Event().Text),
				User:            user,
				Bundle:          bundle,
				Channel:         channel,
			})
			if err != nil {
				response.Reply(err.Error())
				return
			}
			response.Reply(msg)
		},
	})

	slack.Command("lookup <bundle>", &slacker.CommandDefinition{
		Description: "Get info about a bundle.",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			bundle, err := parseBundleName(request.StringParam("bundle", ""))
			if err != nil {
				response.Reply(err.Error())
				return
			}
			msg, err := manager.LookupBundle(bundle)
			if err != nil {
				response.Reply(err.Error())
				return
			}
			response.Reply(msg)
		},
	})
	slack.Command("list", &slacker.CommandDefinition{
		Description: "See who is hogging all the clusters.",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			response.Reply(manager.ListClusters(request.Event().User))
		},
	})
	slack.Command("refresh", &slacker.CommandDefinition{
		Description: "If the cluster is currently marked as failed, retry fetching its credentials in case of an error.",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			user := request.Event().User
			channel := request.Event().Channel
			if !isDirectMessage(channel) {
				response.Reply("you must direct message me this request")
				return
			}
			msg, err := manager.SyncClusterForUser(user)
			if err != nil {
				response.Reply(err.Error())
				return
			}
			response.Reply(msg)
		},
	})
	slack.Command("done", &slacker.CommandDefinition{
		Description: "Terminate the running cluster",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			user := request.Event().User
			channel := request.Event().Channel
			if !isDirectMessage(channel) {
				response.Reply("you must direct message me this request")
				return
			}
			msg, err := manager.TerminateClusterForUser(user)
			if err != nil {
				response.Reply(err.Error())
				return
			}
			response.Reply(msg)
		},
	})

	slack.Command("auth", &slacker.CommandDefinition{
		Description: "Send the credentials for the cluster you most recently requested",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			user := request.Event().User
			channel := request.Event().Channel
			if !isDirectMessage(channel) {
				response.Reply("you must direct message me this request")
				return
			}
			job, err := manager.GetLaunchCluster(user)
			if err != nil {
				response.Reply(err.Error())
				return
			}
			job.RequestedChannel = channel
			b.notifyCluster(slacker.NewResponse(request.Event(), slack.Client(), slack.RTM()), job)
		},
	})

	slack.Command("version", &slacker.CommandDefinition{
		Description: "Report the version of the bot",
		Handler: func(request slacker.Request, response slacker.ResponseWriter) {
			response.Reply(fmt.Sprintf("Running `%s` from https://github.com/bbrowning/crc-cluster-bot", version.Get().String()))
		},
	})

	klog.Infof("crc-cluster-bot up and listening to slack")
	return slack.Listen(context.Background())
}

func (b *Bot) jobResponder(s *slacker.Slacker) func(Job) {
	return func(job Job) {
		if len(job.RequestedChannel) == 0 || len(job.RequestedBy) == 0 {
			klog.Infof("job %q has no requested channel or user, can't notify", job.Name)
			return
		}
		if len(job.Credentials) == 0 && len(job.Failure) == 0 {
			klog.Infof("no credentials or failure, still pending")
			return
		}
		b.notifyCluster(slacker.NewResponse(&slack.MessageEvent{Msg: slack.Msg{Channel: job.RequestedChannel}}, s.Client(), s.RTM()), &job)
	}
}

func (b *Bot) notifyCluster(response slacker.ResponseWriter, cluster *Job) {
	switch {
	case len(cluster.Failure) > 0:
		response.Reply(fmt.Sprintf("your cluster failed to launch: %s", cluster.Failure))
	case len(cluster.Credentials) == 0:
		response.Reply(fmt.Sprintf("cluster is still starting (launched %d minutes ago)", time.Now().Sub(cluster.RequestedAt)/time.Minute))
	default:
		comment := fmt.Sprintf(
			"Your cluster is ready, it will be shut down automatically in ~%d minutes.",
			cluster.ExpiresAt.Sub(time.Now())/time.Minute,
		)
		if len(cluster.PasswordSnippet) > 0 {
			comment += "\n" + cluster.PasswordSnippet
		}
		b.sendKubeconfig(response, cluster.RequestedChannel, cluster.Credentials, comment, cluster.RequestedAt.Format("2006-01-02-150405"))
	}
}

func (b *Bot) sendKubeconfig(response slacker.ResponseWriter, channel, contents, comment, identifier string) {
	_, err := response.Client().UploadFile(slack.FileUploadParameters{
		Content:        contents,
		Channels:       []string{channel},
		Filename:       fmt.Sprintf("cluster-bot-%s.kubeconfig", identifier),
		Filetype:       "text",
		InitialComment: comment,
	})
	if err != nil {
		klog.Infof("error: unable to send attachment with message: %v", err)
		return
	}
	klog.Infof("successfully uploaded file to %s", channel)
}

type slackResponse struct {
	Ok    bool
	Error string
}

func isRetriable(err error) bool {
	// there are several conditions that result from closing the connection on our side
	switch {
	case err == nil,
		err == io.EOF,
		strings.Contains(err.Error(), "use of closed network connection"):
		return true
	case strings.Contains(err.Error(), "cannot unmarshal object into Go struct field"):
		// this could be a legitimate error, so log it to ensure we can debug
		klog.Infof("warning: Ignoring serialization error and continuing: %v", err)
		return true
	default:
		return false
	}
}

func isDirectMessage(channel string) bool {
	return strings.HasPrefix(channel, "D")
}

func codeSlice(items []string) []string {
	code := make([]string, 0, len(items))
	for _, item := range items {
		code = append(code, fmt.Sprintf("`%s`", item))
	}
	return code
}

func parseBundleName(input string) (string, error) {
	input = strings.TrimSpace(input)
	if len(input) == 0 {
		return "", nil
	}
	input = stripLinks(input)
	return input, nil
}

func stripLinks(input string) string {
	var b strings.Builder
	for {
		open := strings.Index(input, "<")
		if open == -1 {
			b.WriteString(input)
			break
		}
		close := strings.Index(input[open:], ">")
		if close == -1 {
			b.WriteString(input)
			break
		}
		pipe := strings.Index(input[open:], "|")
		if pipe == -1 || pipe > close {
			b.WriteString(input[0:open])
			b.WriteString(input[open+1 : open+close])
			input = input[open+close+1:]
			continue
		}
		b.WriteString(input[0:open])
		b.WriteString(input[open+pipe+1 : open+close])
		input = input[open+close+1:]
	}
	return b.String()
}
