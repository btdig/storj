// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

// +build windows
// +build ignore

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"storj.io/storj/private/testcontext"
)

var (
	releaseMSIPath string
	// TODO: make this more dynamic and/or use versioncontrol server?
	// (NB: can't use versioncontrol server until updater process is added to response)
	oldRelease  = "v0.26.2"
	msiBaseArgs = []string{
		"/quiet", "/qn",
		"/norestart",
	}

	msiPathFlag = flag.String("msi", "", "path to the msi to use")
)

func TestMain(m *testing.M) {
	flag.Parse()
	if *msiPathFlag == "" {
		log.Fatal("no msi passed, use `go test ... -args -msi \"<msi path (using \\ seperator)>\"`")
	}

	tmp, err := ioutil.TempDir("", "release")
	if err != nil {
		panic(err)
	}

	releaseMSIPath = filepath.Join(tmp, oldRelease+".msi")
	if err = downloadInstaller(oldRelease, releaseMSIPath); err != nil {
		panic(err)
	}
	defer func() {
		err = errs.Combine(os.RemoveAll(tmp))
	}()

	os.Exit(m.Run())
}

func TestInstaller_Config(t *testing.T) {
	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	installDir := ctx.Dir("install")
	configPath := ctx.File("install", "config.yaml")

	walletAddr := "0x0000000000000000000000000000000000000000"
	email := "user@mail.test"
	publicAddr := "127.0.0.1:10000"

	args := []string{
		fmt.Sprintf("STORJ_WALLET=%s", walletAddr),
		fmt.Sprintf("STORJ_EMAIL=%s", email),
		fmt.Sprintf("STORJ_PUBLIC_ADDRESS=%s", publicAddr),
	}
	install(t, ctx, *msiPathFlag, installDir, args...)
	defer requireUninstall(t, ctx)

	expectedEmail := fmt.Sprintf("operator.email: %s", email)
	expectedWallet := fmt.Sprintf("operator.wallet: \"%s\"", walletAddr)
	expectedAddr := fmt.Sprintf("contact.external-address: %s", publicAddr)

	configStr := readConfigLines(t, ctx, configPath)

	require.Contains(t, configStr, expectedEmail)
	require.Contains(t, configStr, expectedWallet)
	require.Contains(t, configStr, expectedAddr)
}

func TestUpgrade_Config(t *testing.T) {
	if *msiPathFlag == "" {
		t.Fatal("no msi passed, use `go test ... -args -msi <msi path>`")
	}

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	installDir := ctx.Dir("install")
	configPath := ctx.File("install", "config.yaml")

	walletAddr := "0x0000000000000000000000000000000000000000"
	email := "user@mail.test"
	publicAddr := "127.0.0.1:10000"

	// install old release
	args := []string{
		fmt.Sprintf("STORJ_WALLET=%s", walletAddr),
		fmt.Sprintf("STORJ_EMAIL=%s", email),
		fmt.Sprintf("STORJ_PUBLIC_ADDRESS=%s", publicAddr),
	}
	install(t, ctx, releaseMSIPath, installDir, args...)

	// upgrade using test msi
	install(t, ctx, *msiPathFlag, installDir)
	defer requireUninstall(t, ctx)

	expectedEmail := fmt.Sprintf("operator.email: %s", email)
	expectedWallet := fmt.Sprintf("operator.wallet: \"%s\"", walletAddr)
	expectedAddr := fmt.Sprintf("contact.external-address: %s", publicAddr)

	configStr := readConfigLines(t, ctx, configPath)

	require.Contains(t, configStr, expectedEmail)
	require.Contains(t, configStr, expectedWallet)
	require.Contains(t, configStr, expectedAddr)
}

func install(t *testing.T, ctx *testcontext.Context, msiPath, installDir string, args ...string) {
	t.Logf("installing from %s\n", msiPath)
	logPath := ctx.File("log", "install.log")
	args = append(append([]string{
		"/i", msiPath,
		"/log", logPath,
	}, append(msiBaseArgs, "INSTALLFOLDER="+installDir)...), args...)

	installOut, err := exec.Command("msiexec", args...).CombinedOutput()
	if !assert.NoError(t, err) {
		installLogData, err := ioutil.ReadFile(logPath)
		if assert.NoError(t, err) {
			t.Logf("MSIExec log:\n============================\n%s", string(installLogData))
		}
		t.Logf("MSIExec output:\n============================\n%s", string(installOut))
		t.Fatal()
	}
}

func requireUninstall(t *testing.T, ctx *testcontext.Context) {
	logPath := ctx.File("log", "uninstall.log")
	uninstallOut, err := uninstall(logPath)
	if !assert.NoError(t, err) {
		uninstallLogData, err := ioutil.ReadFile(logPath)
		if !assert.NoError(t, err) {
			t.Logf("MSIExec log:\n============================\n%s\n", string(uninstallLogData))
		}
		t.Fatalf("MSIExec output:\n============================\n%s", string(uninstallOut))
	}
}

func uninstall(logPath string) ([]byte, error) {
	if err := stopServices("storagenode", "storagenode-updater"); err != nil {
		return []byte{}, err
	}

	args := append([]string{"/uninstall", *msiPathFlag, "/log", logPath}, msiBaseArgs...)
	return exec.Command("msiexec", args...).CombinedOutput()
}

func stopServices(names ...string) (err error) {
	serviceMgr, err := mgr.Connect()
	if err != nil {
		return err
	}

	group := new(errgroup.Group)
	for _, name := range names {
		name := name
		group.Go(func() (err error) {
			service, err := serviceMgr.OpenService(name)
			if err != nil {
				return err
			}
			defer func() {
				err = errs.Combine(err, service.Close())
			}()

			if _, err = service.Control(svc.Stop); err != nil {
				return err
			}
			return waitForStop(service)
		})
	}

	return group.Wait()
}

func waitForStop(service *mgr.Service) error {
	ctx, _ := context.WithTimeout(context.Background(), 30*time.Second)
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		switch status.State {
		case svc.Stopped:
			return nil
		default:
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func readConfigLines(t *testing.T, ctx *testcontext.Context, configPath string) string {
	configFile, err := os.Open(configPath)
	require.NoError(t, err)
	defer ctx.Check(configFile.Close)

	// NB: strip empty lines and comments
	configBuf := new(bytes.Buffer)
	scanner := bufio.NewScanner(configFile)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.Trim(line, " \t\n")
		out := append(scanner.Bytes(), byte('\n'))
		if len(line) == 0 {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			_, err := configBuf.Write(out)
			require.NoError(t, err)
		}
	}
	if err := scanner.Err(); err != nil {
		require.NoError(t, err)
	}
	return configBuf.String()
}

func releaseUrl(version, name, ext string) string {
	urlTemplate := "https://github.com/storj/storj/releases/download/{version}/{service}_{os}_{arch}{ext}.zip"

	url := strings.Replace(urlTemplate, "{version}", version, 1)
	url = strings.Replace(url, "{service}", name, 1)
	url = strings.Replace(url, "{os}", runtime.GOOS, 1)
	url = strings.Replace(url, "{arch}", runtime.GOARCH, 1)
	url = strings.Replace(url, "{ext}", ext, 1)
	return url
}

func downloadInstaller(version, dst string) error {
	zipDir, err := ioutil.TempDir("", "archive")
	if err != nil {
		return err
	}
	zipPath := filepath.Join(zipDir, version+".msi.zip")

	url := releaseUrl(version, "storagenode", ".msi")

	if err := downloadArchive(url, zipPath); err != nil {
		return err
	}
	if err := unpackBinary(zipPath, releaseMSIPath); err != nil {
		return err
	}
	return nil
}

func downloadArchive(url, dst string) (err error) {
	resp, err := http.Get(url)
	defer func() {
		err = errs.Combine(err, resp.Body.Close())
	}()

	if resp.StatusCode != http.StatusOK {
		return errs.New("error downloading %s; non-success status: %s", url, resp.Status)
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer func() {
		err = errs.Combine(err, dstFile.Close())
	}()

	if _, err = io.Copy(dstFile, resp.Body); err != nil {
		return err
	}
	return nil
}

func unpackBinary(archive, dst string) (err error) {
	zipReader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() {
		err = errs.Combine(err, zipReader.Close())
	}()

	zipedExec, err := zipReader.File[0].Open()
	if err != nil {
		return err
	}
	defer func() {
		err = errs.Combine(err, zipedExec.Close())
	}()

	newExec, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer func() {
		err = errs.Combine(err, newExec.Close())
	}()

	_, err = io.Copy(newExec, zipedExec)
	if err != nil {
		return err
	}
	return nil
}