package source

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestSource(t *testing.T) {
	info, err := ModuleInfo(context.TODO(), NewClient(5*time.Second), "github.com/goproxyio/goproxy", "latest")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(info)
}
