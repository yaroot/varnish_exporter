package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/gorilla/handlers"
	"github.com/valyala/fastjson"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

type Metric struct {
	Category string
	Name     string
	Labels   map[string]string
	Value    *fastjson.Value
}

func newMetric(cate, name string, labels map[string]string, value *fastjson.Value) Metric {
	return Metric{
		Category: cate,
		Name:     name,
		Labels:   labels,
		Value:    value,
	}
}

func newLabel(name, value string) map[string]string {
	return map[string]string{
		name: value,
	}
}

func emptyLabel() map[string]string {
	return map[string]string{}
}

func formatLabel(labels map[string]string) string {
	var w bytes.Buffer

	for name, value := range labels {
		if w.Len() == 0 {
			w.WriteString("{")
		} else {
			w.WriteString(",")
		}

		w.WriteString(name)
		w.WriteString("=\"")
		w.WriteString(value)
		w.WriteString("\"")
	}
	if w.Len() > 0 {
		w.WriteString("}")
	}
	return w.String()
}

func genMetrics(value *fastjson.Object, activeVCL string) string {
	metrics := make([]Metric, 0)

	value.Visit(func(key0 []byte, val *fastjson.Value) {
		key := string(key0)
		if key == "version" {
			return
		}
		splits := strings.Split(key, ".")
		if len(splits) < 2 {
			return
		}
		prefix := splits[0]
		if prefix == "VBE" {
			fmt.Println(key)
			vclVersion := splits[1]
			if activeVCL == "" {
				activeVCL = vclVersion
			}
			if vclVersion != activeVCL {
				return
			}
			backend := splits[2]
			name := splits[3]
			metrics = append(metrics, newMetric("backend", name, newLabel("name", backend), val))
		} else if prefix == "MEMPOOL" {
			pool := splits[1]
			name := splits[2]
			metrics = append(metrics, newMetric("mempool", name, newLabel("name", pool), val))
		} else if prefix == "SMA" {
			typ := strings.ToLower(splits[1])
			name := splits[2]
			metrics = append(metrics, newMetric("sma", name, newLabel("type", typ), val))
		} else if prefix == "LCK" {
			target := splits[1]
			name := splits[2]
			metrics = append(metrics, newMetric("lock", name, newLabel("target", target), val))
		} else if prefix == "SMF" {
			typ := splits[1]
			name := splits[2]
			metrics = append(metrics, newMetric("smf", name, newLabel("type", typ), val))
		} else if prefix == "MAIN" {
			name := splits[1]
			metrics = append(metrics, newMetric("main", name, emptyLabel(), val))
		} else if prefix == "MGT" {
			name := splits[1]
			metrics = append(metrics, newMetric("mgt", name, emptyLabel(), val))
		} else {
			log.Println("Unknown metrics", key, val)
		}
	})

	var w bytes.Buffer

	for _, m := range metrics {
		desc := string(m.Value.GetStringBytes("description"))
		_flag := string(m.Value.GetStringBytes("flag"))
		v := m.Value.GetInt("value")

		_type := "counter"
		if _flag == "g" {
			_type = "gauge"
		}

		name := fmt.Sprintf("varnish_%s_%s", m.Category, m.Name)

		w.WriteString(fmt.Sprintf("# HELP %s %s\n", name, desc))
		w.WriteString(fmt.Sprintf("# TYPE %s %s\n", name, _type))
		w.WriteString(fmt.Sprintf("%s%s %d\n", name, formatLabel(m.Labels), v))
	}

	w.WriteString("# HELP varnish_active_vcl vcl version\n")
	w.WriteString("# TYPE varnish_active_vcl gauge\n")
	w.WriteString(fmt.Sprintf("varnish_active_vcl{version=\"%s\"} 1\n", activeVCL))

	return w.String()
}

func main() {
	bind := flag.String("bind", ":9131", "binding address")
	check := flag.Bool("check", false, "print metrics and exit")
	noAdmin := flag.Bool("no-admin", false, "do not call 'varnishadm'")
	flag.Parse()

	if *check {
		out, err := collectMetrics(*noAdmin)
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Println(out)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/metrics")
		w.WriteHeader(302)
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		out, err := collectMetrics(*noAdmin)
		if err != nil {
			log.Println(err)
			w.WriteHeader(500)
			_, _ = w.Write([]byte(err.Error()))
			return
		} else {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte(out))
		}
	})

	log.Printf("Starting varnish_exporter on %s\n", *bind)
	err := http.ListenAndServe(*bind, handlers.CombinedLoggingHandler(os.Stdout, mux))
	if err != nil {
		log.Fatalln(err)
	}
}

func collectMetrics(noAdmin bool) (string, error) {
	activeVCL, err := listVCL(noAdmin)
	if err != nil {
		return "", err
	}
	stats, err := collectStats()
	if err != nil {
		return "", err
	}
	return genMetrics(stats, activeVCL), nil
}

func collectStats() (*fastjson.Object, error) {
	out, err := execute("varnishstat", "-j", "-t", "0")
	if err != nil {
		return nil, err
	}
	val, err := fastjson.Parse(out)
	if err != nil {
		return nil, err
	}
	return val.Object()
}

func listVCL(noAdmin bool) (string, error) {
	if noAdmin {
		return "", nil
	}
	out, err := execute("varnishadm", "vcl.list", "-j")
	if err != nil {
		return "", err
	}
	return parseVCLList(out)
}

func execute(command string, params ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(command, params...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func parseVCLList(input string) (string, error) {
	val, err := fastjson.Parse(input)
	if err != nil {
		return "", err
	}

	arr, err := val.Array()
	if err != nil {
		return "", err
	}

	for _, value := range arr[3:] {
		st := string(value.GetStringBytes("status"))
		if st == "active" {
			name := string(value.GetStringBytes("name"))
			return name, nil
		}
	}
	return "", errors.New("No active ACL found")
}
