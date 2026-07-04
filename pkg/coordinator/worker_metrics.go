package coordinator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var workerMetricsHTTPClient = &http.Client{Timeout: 1200 * time.Millisecond}

type workerMetricsTarget struct {
	tag string
	url string
}

func (h *Hub) writeWorkerMetrics(ctx context.Context, out io.Writer) {
	targets := h.workerMetricsTargets()
	for _, target := range targets {
		if target.url == "" {
			continue
		}
		if err := h.writeWorkerMetricsFromTarget(ctx, out, target); err != nil {
			h.log.Debug().Err(err).Str("worker", target.tag).Str("url", target.url).Msg("worker metrics scrape skipped")
		}
	}
}

func (h *Hub) workerMetricsTargets() []workerMetricsTarget {
	targets := make([]workerMetricsTarget, 0)
	if h == nil {
		return targets
	}
	for worker := range h.workers.Values() {
		if worker == nil || strings.TrimSpace(worker.MonitoringURL) == "" {
			continue
		}
		tag := strings.TrimSpace(worker.Tag)
		if tag == "" {
			tag = strings.TrimSpace(worker.Zone)
		}
		if tag == "" {
			tag = worker.Id().String()
		}
		targets = append(targets, workerMetricsTarget{tag: tag, url: worker.MonitoringURL})
	}
	return targets
}

func (h *Hub) writeWorkerMetricsFromTarget(ctx context.Context, out io.Writer, target workerMetricsTarget) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.url, nil)
	if err != nil {
		return err
	}
	resp, err := workerMetricsHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("worker metrics returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if line, ok := workerMetricSampleLine(scanner.Text(), target.tag); ok {
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func workerMetricSampleLine(raw string, tag string) (string, bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	name := prometheusMetricName(line)
	if !strings.HasPrefix(name, "cloud_game_nds_") && !strings.HasPrefix(name, "cloud_game_worker_") {
		return "", false
	}
	if strings.HasPrefix(name, "cloud_game_nds_") {
		line = addPrometheusLabel(line, "worker", tag)
	}
	return line, true
}

func prometheusMetricName(line string) string {
	end := strings.IndexAny(line, "{ \t")
	if end < 0 {
		return line
	}
	return line[:end]
}

func addPrometheusLabel(line string, key string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return line
	}
	sampleEnd := strings.IndexAny(line, " \t")
	if sampleEnd < 0 {
		return line
	}

	metric := line[:sampleEnd]
	if strings.Contains(metric, key+"=") {
		return line
	}
	rest := line[sampleEnd:]
	label := key + "=" + strconv.Quote(value)

	open := strings.Index(metric, "{")
	close := strings.LastIndex(metric, "}")
	if open >= 0 && close > open {
		if close == open+1 {
			metric = metric[:open+1] + label + metric[close:]
		} else {
			metric = metric[:close] + "," + label + metric[close:]
		}
	} else {
		metric += "{" + label + "}"
	}
	return metric + rest
}
