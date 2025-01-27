package client

import (
	"fmt"

	"github.com/keybase/cli"
	"github.com/keybase/client/go/libcmdline"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/chat1"
	"github.com/keybase/client/go/protocol/keybase1"
	"golang.org/x/net/context"
)

type CmdChatArchiveResume struct {
	libkb.Contextified
	jobID chat1.ArchiveJobID
}

func NewCmdChatArchiveResumeRunner(g *libkb.GlobalContext) *CmdChatArchiveResume {
	return &CmdChatArchiveResume{
		Contextified: libkb.NewContextified(g),
	}
}

func newCmdChatArchiveResume(cl *libcmdline.CommandLine, g *libkb.GlobalContext) cli.Command {
	return cli.Command{
		Name:         "archive-resume",
		Usage:        "Continue a paused archive job",
		ArgumentHelp: "job-id",
		Action: func(c *cli.Context) {
			cl.ChooseCommand(NewCmdChatArchiveResumeRunner(g), "archive-resume", c)
			cl.SetLogForward(libcmdline.LogForwardNone)
		},
	}
}

func (c *CmdChatArchiveResume) Run() error {
	client, err := GetChatLocalClient(c.G())
	if err != nil {
		return err
	}

	arg := chat1.ArchiveChatResumeArg{
		JobID:            c.jobID,
		IdentifyBehavior: keybase1.TLFIdentifyBehavior_CHAT_CLI,
	}

	err = client.ArchiveChatResume(context.TODO(), arg)
	if err != nil {
		return err
	}

	ui := c.G().UI.GetTerminalUI()
	ui.Printf("Job resumed\n")

	return nil
}

func (c *CmdChatArchiveResume) ParseArgv(ctx *cli.Context) (err error) {
	if len(ctx.Args()) != 1 {
		return fmt.Errorf("job-id is required")
	}
	c.jobID = chat1.ArchiveJobID(ctx.Args().Get(0))
	return nil
}

func (c *CmdChatArchiveResume) GetUsage() libkb.Usage {
	return libkb.Usage{
		Config: true,
		API:    true,
	}
}
