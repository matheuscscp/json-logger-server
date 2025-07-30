package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v2"
)

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339Nano,
	})

	var remoteLoggers map[string]*struct {
		HTTP *struct {
			Method string `yaml:"method"`
			URL    string `yaml:"url"`
			Auth   *struct {
				BasicAuth *struct {
					UsernameFile string `yaml:"usernameFile"`
					PasswordFile string `yaml:"passwordFile"`
				} `yaml:"basic,omitempty"`
			} `yaml:"auth,omitempty"`
			Headers http.Header `yaml:"headers,omitempty"`
			Body    *struct {
				Templates []string `yaml:"templates,omitempty"`

				templates []*template.Template `yaml:"-"` // Used internally to store the parsed templates.
			} `yaml:"body,omitempty"`
		} `yaml:"http,omitempty"`
	}
	remoteLoggersPath := "/etc/json-logger-server/remote-loggers.yaml"
	if v := os.Getenv("REMOTE_LOGGERS_PATH"); v != "" {
		remoteLoggersPath = v
	}
	switch f, err := os.Open(remoteLoggersPath); {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		logrus.WithError(err).Fatalf("failed to open file %s", remoteLoggersPath)
	case err == nil:
		if err := yaml.NewDecoder(f).Decode(&remoteLoggers); err != nil {
			logrus.WithError(err).Fatalf("failed to decode file %s as YAML", remoteLoggersPath)
		}
		f.Close()
		for name, remoteLogger := range remoteLoggers {
			if remoteLogger.HTTP != nil && remoteLogger.HTTP.Body != nil {
				tplFuncs := template.FuncMap{
					"urlencode": url.QueryEscape,
					"tolower":   strings.ToLower,
					"get": func(m map[string][]string, key string) string {
						s := m[key]
						if len(s) == 0 {
							return ""
						}
						return s[0]
					},
				}
				for i, templateString := range remoteLogger.HTTP.Body.Templates {
					tpl := template.New(fmt.Sprintf("%s-body-%d", name, i))
					tpl.Funcs(tplFuncs)
					tpl, err := tpl.Parse(templateString)
					if err != nil {
						logrus.WithError(err).Fatalf("failed to parse body template number %d for remote logger %s", i, name)
					}
					remoteLogger.HTTP.Body.templates = append(remoteLogger.HTTP.Body.templates, tpl)
				}
			}
		}
	}

	respondError := func(w http.ResponseWriter, status int, err error, l logrus.FieldLogger, msg string) {
		l.WithError(err).Error(msg)
		w.WriteHeader(status)
		if b, jsonErr := json.Marshal(map[string]string{
			"error":   err.Error(),
			"message": msg,
		}); jsonErr != nil {
			l.WithError(jsonErr).Error("failed to marshal error response")
		} else {
			if _, writeErr := w.Write(b); writeErr != nil {
				l.WithError(writeErr).Error("failed to write error response")
			}
		}
	}

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}
	s := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusOK)
				return
			}

			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}

			b, err := io.ReadAll(r.Body)
			if err != nil {
				respondError(w, http.StatusBadRequest, err, logrus.StandardLogger(),
					"failed to read request body")
				return
			}
			var body any
			if err := json.Unmarshal(b, &body); err != nil {
				respondError(w, http.StatusBadRequest, err, logrus.StandardLogger(),
					"failed to unmarshal request body")
				return
			}

			logrus.WithField("body", body).Info("request")

			for _, remoteLogger := range remoteLoggers {
				l := logrus.WithField("remoteLogger", remoteLogger)
				switch {
				case remoteLogger.HTTP != nil:
					httpRemote := remoteLogger.HTTP

					var remoteBody io.Reader
					if httpRemote.Body != nil {
						switch {
						case len(httpRemote.Body.templates) > 0:
							data := map[string]any{
								"host":    r.Host,
								"headers": r.Header,
								"method":  r.Method,
								"path":    r.URL.Path,
								"query":   r.URL.Query(),
								"body":    body,
							}
							var executedTemplates []string
							for _, tpl := range httpRemote.Body.templates {
								data["executedTemplates"] = executedTemplates
								var buf strings.Builder
								if err := tpl.Execute(&buf, data); err != nil {
									respondError(w, http.StatusInternalServerError, err, l,
										"failed to execute body template")
									return
								}
								executedTemplates = append(executedTemplates, buf.String())
							}
							remoteBody = strings.NewReader(executedTemplates[len(executedTemplates)-1])
						}
					}

					req, err := http.NewRequestWithContext(r.Context(),
						httpRemote.Method, httpRemote.URL, remoteBody)
					if err != nil {
						respondError(w, http.StatusInternalServerError, err, l,
							"failed to create HTTP request")
						return
					}

					if auth := httpRemote.Auth; auth != nil {
						switch basicAuth := auth.BasicAuth; {
						case basicAuth != nil:
							username, err := os.ReadFile(basicAuth.UsernameFile)
							if err != nil {
								respondError(w, http.StatusInternalServerError, err, l,
									"failed to read basic auth username file")
								return
							}
							password, err := os.ReadFile(basicAuth.PasswordFile)
							if err != nil {
								respondError(w, http.StatusInternalServerError, err, l,
									"failed to read basic auth password file")
								return
							}
							req.SetBasicAuth(string(username), string(password))
						}
					}

					maps.Copy(req.Header, httpRemote.Headers)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						respondError(w, http.StatusInternalServerError, err, l, "failed to send HTTP request")
						continue
					}
					resp.Body.Close()
				}
			}
		}),
	}

	go s.ListenAndServe()
	logrus.Info("server started")

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	logrus.Info("waiting for signal")
	<-ch
	logrus.Info("signal received, shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Shutdown(ctx)
}
