package notifications

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ClickHouse is infrastructure for the released app, not a BuildBuddy build.
// Fetch only the pinned upstream static binary; no Docker Hub image is used.
func startOLAP(t *testing.T, ip string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	request, err := http.NewRequestWithContext(ctx, "GET", "https://packages.clickhouse.com/tgz/stable/clickhouse-common-static-24.8.14.39-amd64.tgz", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, 200, response.StatusCode)
	archive, err := os.Create(filepath.Join(dir, "clickhouse.tgz"))
	require.NoError(t, err)
	hash := sha512.New()
	_, err = io.Copy(io.MultiWriter(archive, hash), response.Body)
	require.NoError(t, err)
	require.Equal(t, "f7ff660df46e40e4aa94dc360a00386f3f21030a76366e080ed1c3ae2c91b1754debe745459bf8ee1881bb509066dd062b8b76ec983bfba463cefff18b1854e4", hex.EncodeToString(hash.Sum(nil)))
	_, err = archive.Seek(0, 0)
	require.NoError(t, err)
	gz, err := gzip.NewReader(archive)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	binary := filepath.Join(dir, "clickhouse")
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if h.Name != "clickhouse-common-static-24.8.14.39/usr/bin/clickhouse" {
			continue
		}
		file, err := os.OpenFile(binary, os.O_CREATE|os.O_WRONLY, 0755)
		require.NoError(t, err)
		_, err = io.Copy(file, tr)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		found = true
		break
	}
	require.True(t, found)
	require.NoError(t, gz.Close())
	require.NoError(t, archive.Close())
	freePort := func() int {
		l, err := net.Listen("tcp", "0.0.0.0:0")
		require.NoError(t, err)
		p := l.Addr().(*net.TCPAddr).Port
		require.NoError(t, l.Close())
		return p
	}
	tcp, web := freePort(), freePort()
	config := fmt.Sprintf(`<clickhouse>
 <logger><level>warning</level><console>true</console></logger>
 <path>%s/data/</path><tmp_path>%s/tmp/</tmp_path><user_files_path>%s/user_files/</user_files_path>
 <listen_host>0.0.0.0</listen_host><tcp_port>%d</tcp_port><http_port>%d</http_port>
 <max_server_memory_usage>536870912</max_server_memory_usage><max_thread_pool_size>128</max_thread_pool_size>
 <background_pool_size>4</background_pool_size><background_schedule_pool_size>4</background_schedule_pool_size>
 <background_message_broker_schedule_pool_size>2</background_message_broker_schedule_pool_size><background_distributed_schedule_pool_size>2</background_distributed_schedule_pool_size>
 <merge_tree><number_of_free_entries_in_pool_to_execute_optimize_entire_partition>2</number_of_free_entries_in_pool_to_execute_optimize_entire_partition><number_of_free_entries_in_pool_to_execute_mutation>2</number_of_free_entries_in_pool_to_execute_mutation><number_of_free_entries_in_pool_to_lower_max_size_of_merge>2</number_of_free_entries_in_pool_to_lower_max_size_of_merge></merge_tree>
 <users><default><password></password><networks><ip>::/0</ip></networks><profile>default</profile><quota>default</quota></default></users>
 <profiles><default><max_threads>2</max_threads><max_memory_usage>268435456</max_memory_usage></default></profiles><quotas><default/></quotas>
</clickhouse>`, dir, dir, dir, tcp, web)
	configPath := filepath.Join(dir, "config.xml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0600))
	logDir := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR")
	if logDir == "" {
		logDir = dir
	}
	log, err := os.Create(filepath.Join(logDir, "notifications-clickhouse.log"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			b, err := os.ReadFile(log.Name())
			if err == nil {
				t.Logf("ClickHouse log: %s", b)
			}
		}
	})
	cmd := exec.Command(binary, "server", "--config-file="+configPath)
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = log.Close()
	})
	require.Eventually(t, func() bool {
		client := &http.Client{Timeout: time.Second}
		r, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/ping", web))
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	}, 30*time.Second, 100*time.Millisecond)
	return fmt.Sprintf("clickhouse://default:@%s:%d/default", ip, tcp)
}
