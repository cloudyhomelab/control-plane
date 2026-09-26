// Package testutil holds helpers shared by tests.
package testutil

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// FakeToolEnv, when set in a child's environment, makes the test binary act as a tool.
const FakeToolEnv = "CONTROLPLANE_FAKE_TOOL"

// RunFakeTool must be called first in TestMain. When the binary was started as a fake tool
// it behaves like terraform/packer/ansible according to a "fake-mode" file in its working
// directory ("ok", "fail", or "hang"), then exits.
func RunFakeTool() {
	if os.Getenv(FakeToolEnv) == "" {
		return
	}
	args := os.Args[1:]
	mode := "ok"
	if b, err := os.ReadFile("fake-mode"); err == nil {
		mode = strings.TrimSpace(string(b))
	}
	fmt.Printf("fake %s\n", strings.Join(args, " "))
	if s := os.Getenv("FAKE_SECRET"); s != "" {
		fmt.Printf("secret is %s\n", s)
	}
	if len(args) == 0 {
		os.Exit(0)
	}
	switch args[0] {
	case "plan":
		if mode == "fail" {
			fmt.Fprintln(os.Stderr, "Error: plan failed")
			os.Exit(3)
		}
		if mode == "hang" {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGINT)
			select {
			case <-sig:
				fmt.Println("interrupted, releasing lock")
				os.Exit(130)
			case <-time.After(30 * time.Second):
			}
		}
		for _, a := range args {
			if out, ok := strings.CutPrefix(a, "-out="); ok {
				os.WriteFile(out, []byte("PLAN"), 0o644)
			}
		}
	case "show":
		fmt.Println(`{"format_version":"1.2"}`)
	case "apply":
		b, err := os.ReadFile(args[len(args)-1])
		if err != nil || string(b) != "PLAN" {
			fmt.Fprintln(os.Stderr, "bad plan file")
			os.Exit(1)
		}
		fmt.Println("Apply complete!")
	}
	os.Exit(0)
}
