package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jitsucom/bulker/sync-sidecar/db"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type SideCar struct {
	syncId     string
	taskId     string
	storageKey string
	command    string

	packageName    string
	packageVersion string

	stdOutPipeFile string
	stdErrPipeFile string
	logsConnection string

	bulkerURL       string
	bulkerAuthToken string

	databaseURL string
	dbpool      *pgxpool.Pool

	startedAt time.Time

	//first error occurred during sync
	firstErr error
	// write stream of io.Pipe. Bulker reads from this read stream of this pipe
	currentStream     *io.PipeWriter
	currentStreamName string
	processedStreams  map[string]int
	catalog           map[string]*Stream
	initialState      string
	state             *StateRow
	eventsCounter     int
	streamsWaitGroup  sync.WaitGroup
}

func main() {
	startedAt, err := time.Parse(time.RFC3339, os.Getenv("STARTED_AT"))
	if err != nil {
		startedAt = time.Now()
	}

	sidecar := &SideCar{
		syncId:          os.Getenv("SYNC_ID"),
		taskId:          os.Getenv("TASK_ID"),
		command:         os.Getenv("COMMAND"),
		storageKey:      os.Getenv("STORAGE_KEY"),
		packageName:     os.Getenv("PACKAGE"),
		packageVersion:  os.Getenv("PACKAGE_VERSION"),
		stdOutPipeFile:  os.Getenv("STDOUT_PIPE_FILE"),
		stdErrPipeFile:  os.Getenv("STDERR_PIPE_FILE"),
		logsConnection:  os.Getenv("LOGS_CONNECTION_ID"),
		bulkerURL:       os.Getenv("BULKER_URL"),
		bulkerAuthToken: os.Getenv("BULKER_AUTH_TOKEN"),
		databaseURL:     os.Getenv("DATABASE_URL"),
		startedAt:       startedAt,
	}

	sidecar.Run()

}

func (s *SideCar) Run() {
	//TODO: read catalog to detect full sync or incremental mode
	var err error
	s.dbpool, err = pgxpool.New(context.Background(), s.databaseURL)
	if err != nil {
		s.panic("Unable to create postgres connection pool: %v", err)
	}
	defer s.dbpool.Close()

	defer func() {
		//recover
		if r := recover(); r != nil {
			s.sendStatus(s.command, "FAILED", fmt.Sprint(r))
			os.Exit(1)
		} else if s.isErr() {
			s.sendStatus(s.command, "FAILED", s.firstErr.Error())
			os.Exit(1)
		} else {
			if len(s.processedStreams) > 0 {
				processedStreamsJson, _ := json.Marshal(s.processedStreams)
				s.sendStatus(s.command, "SUCCESS", string(processedStreamsJson))
			} else {
				s.sendStatus(s.command, "SUCCESS", "")
			}
		}
	}()
	s.log("Sidecar. syncId: %s, taskId: %s, package: %s:%s startedAt: %s", s.syncId, s.taskId, s.packageName, s.packageVersion, s.startedAt.Format(time.RFC3339))
	if s.command == "read" {
		//load file from /config/catalog.json and parse it
		err = s.loadCatalog()
		if err != nil {
			s.panic("Error loading catalog: %v", err)
		}
		s.log("Catalog loaded. %d streams selected", len(s.catalog))
		state, ok := s.loadState()
		if ok {
			s.log("State loaded: %s", state)
		}
	}
	var stdOutErrWaitGroup sync.WaitGroup

	errPipe, _ := os.Open(s.stdErrPipeFile)
	defer errPipe.Close()
	stdOutErrWaitGroup.Add(1)
	// read from stderr
	go func() {
		defer stdOutErrWaitGroup.Done()
		scanner := bufio.NewScanner(errPipe)
		scanner.Buffer(make([]byte, 1024*100), 1024*1024*10)
		for scanner.Scan() {
			line := scanner.Text()
			s.sourceLog("ERRSTD", line)
		}
		if err := scanner.Err(); err != nil {
			s.panic("error reading from err pipe: %v", err)
		}
	}()

	s.processedStreams = map[string]int{}
	outPipe, _ := os.Open(s.stdOutPipeFile)
	defer outPipe.Close()
	stdOutErrWaitGroup.Add(1)
	// read from stdout
	go func() {
		defer stdOutErrWaitGroup.Done()

		scanner := bufio.NewScanner(outPipe)
		scanner.Buffer(make([]byte, 1024*100), 1024*1024*10)
		for scanner.Scan() {
			line := scanner.Bytes()
			row := &Row{}
			err := json.Unmarshal(line, row)
			if err != nil {
				s.panic("error parsing airbyte line %s: %v", string(line), err)
			}
			switch row.Type {
			case LogType:
				s.sourceLog(row.Log.Level, row.Log.Message)
			case StateType:
				s.processState(row.State)
			case SpecType:
				s.processSpec(row.Spec)
			case ConnectionStatusType:
				s.processConnectionStatus(row.ConnectionStatus)
			case CatalogType:
				s.processCatalog(row.Catalog)
			case RecordType:
				s.processRecord(row.Record)
			case TraceType:
			default:
				s.panic("not supported type: %s", row.Type)
			}
		}
		if err := scanner.Err(); err != nil {
			s.panic("error reading from pipe: %v", err)
		}
		if s.currentStream != nil {
			if s.isErr() {
				// intentionally break ndjson stream with that error. Bulker will definitely abort this stream
				_, _ = s.currentStream.Write([]byte("ABORT with error: " + s.firstErr.Error() + "\n"))
			} else {
				s.processedStreams[s.currentStreamName] = s.eventsCounter
			}
			_ = s.currentStream.Close()
		}
		s.streamsWaitGroup.Wait()
	}()
	stdOutErrWaitGroup.Wait()
	s.saveState()

}

func (s *SideCar) processState(state *StateRow) {
	s.log("STATE: %+v", state.Data)
	s.state = state
}

func (s *SideCar) saveState() {
	if s.state == nil {
		return
	}
	if s.isErr() {
		s.err("STATE: not saving state because of previous errors")
		return
	}
	stateJson, err := json.Marshal(s.state.Data)
	if err != nil {
		s.panic("error marshalling state %+v: %v", s.state.Data, err)
	}
	s.log("SAVING STATE: %s", stateJson)
	if !s.isErr() {
		s.sendState(string(stateJson))
	}
}

func (s *SideCar) processSpec(spec map[string]any) {
	// ignore previous error messages since we got result
	s.firstErr = nil
	specJson, _ := json.Marshal(spec)
	err := db.UpsertSpec(s.dbpool, s.packageName, s.packageVersion, string(specJson), s.startedAt, "")
	if err != nil {
		s.panic("error updating spec for %s:%s: %v", s.packageName, s.packageVersion, err)
	}
	s.log("Spec: %s", specJson)
}

func (s *SideCar) processConnectionStatus(status *StatusRow) {
	// ignore previous error messages since we got result
	s.firstErr = nil
	s.log("CONNECTION STATUS: %s", joinStrings(status.Status, status.Message))
	st := strings.ReplaceAll(status.Status, "SUCCEEDED", "SUCCESS")
	err := db.UpsertCheck(s.dbpool, s.packageName, s.packageVersion, s.storageKey, st, status.Message, s.startedAt)
	if err != nil {
		s.panic("error updating connection status for: %s: %v", s.storageKey, err)
	}
}

func (s *SideCar) processCatalog(catalog map[string]any) {
	// ignore previous error messages since we got result
	s.firstErr = nil
	catalogJson, _ := json.Marshal(catalog)
	s.log("CATALOG: %s", catalogJson)
	err := db.UpsertCatalog(s.dbpool, s.packageName, s.packageVersion, s.storageKey, string(catalogJson), s.startedAt, "SUCCESS", "")
	if err != nil {
		s.panic("error updating catalog for: %s: %v", s.storageKey, err)
	}
}

func (s *SideCar) processRecord(rec *RecordRow) {
	if s.isErr() {
		//don't send records after first error
		return
	}
	streamName := rec.Stream
	if streamName != s.currentStreamName {
		if s.currentStream != nil {
			s.processedStreams[s.currentStreamName] = s.eventsCounter
			s.eventsCounter = 0
			_ = s.currentStream.Close()
		}
		if _, ok := s.processedStreams[streamName]; ok {
			s.panic("stream '%s' was already processed. We assume that airbyte doesn't mix streams", streamName)
		}
		// we create pipe. everything that is written to w will be sent to bulker via r as reader payload
		r, w := io.Pipe()
		s.currentStream = w
		s.currentStreamName = streamName
		s.streamsWaitGroup.Add(1)
		go func() {
			defer s.streamsWaitGroup.Done()
			str, ok := s.catalog[streamName]
			if !ok {
				s.panic("stream '%s' is not in catalog", streamName)
				return
			}
			s.log("creating stream: %s mode: %s primary keys: %s", streamName, str.SyncMode, str.GetPrimaryKeys())
			mode := "replace_table"
			// if there is no initial sync state, we assume that this is first sync and we need to do full sync
			if str.SyncMode == "incremental" && len(s.initialState) > 0 {
				mode = "batch"
			}
			bulkerUrl := fmt.Sprintf("%s/bulk/%s?tableName=%s&mode=%s&taskId=%s", s.bulkerURL, s.syncId, url.QueryEscape(streamName), mode, s.taskId)
			for _, v := range str.GetPrimaryKeys() {
				bulkerUrl += fmt.Sprintf("&pk=%s", url.QueryEscape(v))
			}
			_, err := s.bulkerRequest(bulkerUrl, r)
			if err != nil {
				s.panic("error sending bulk: %v", err)
				return
			}
			s.log("stream %s: bulker response: OK", streamName)
		}()
	}
	data, err := json.Marshal(rec.Data)
	if err != nil {
		s.panic("error marshalling record: %v", err)
	}
	_, err = s.currentStream.Write(data)
	if err != nil {
		s.panic("error writing to bulk pipe: %v", err)
	}
	_, _ = s.currentStream.Write([]byte("\n"))
	s.eventsCounter++
}

func (s *SideCar) log(message string, args ...any) {
	s._log("jitsu", "INFO", fmt.Sprintf(message, args...))
}

func (s *SideCar) sourceLog(level, message string, args ...any) {
	message = strings.TrimPrefix(message, "INFO ")
	message = strings.TrimPrefix(message, "ERROR ")
	message = strings.TrimPrefix(message, "WARN ")
	message = strings.TrimPrefix(message, "DEBUG ")
	message = strings.TrimPrefix(message, "FATAL ")

	text := fmt.Sprintf(message, args...)
	if (level == "ERROR" || level == "FATAL") && s.firstErr == nil {
		s.firstErr = errors.New(text)
	}
	s._log(s.packageName, level, text)
}

func (s *SideCar) err(message string, args ...any) {
	text := fmt.Sprintf(message, args...)
	if s.firstErr == nil {
		s.firstErr = errors.New(text)
	}
	s._log("jitsu", "ERROR", text)
}

func (s *SideCar) panic(message string, args ...any) {
	text := fmt.Sprintf(message, args...)
	if s.firstErr == nil {
		s.firstErr = errors.New(text)
	}
	s._log("jitsu", "ERROR", text)
	panic(text)
}

func (s *SideCar) isErr() bool {
	return s.firstErr != nil
}

func (s *SideCar) _log(logger, level, message string) {
	fmt.Printf("%s : %s\n", level, message)
	err := s.sendLog(logger, level, message)
	if err != nil {
		fmt.Printf("%s: %v\n", level, err)
	}
}

func (s *SideCar) sendLog(logger, level string, message string) error {
	logMessage := map[string]any{
		"id":        uuid.New().String(),
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"sync_id":   s.syncId,
		"task_id":   s.taskId,
		"logger":    logger,
		"level":     level,
		"message":   message,
	}
	return s.bulkerEvent(s.logsConnection, "task_log", logMessage)
}

func (s *SideCar) sendStatus(command string, status string, description string) {
	s.log("%s %s", strings.ToUpper(command), joinStrings(status, description))
	if command == "read" && s.dbpool != nil {
		err := db.UpsertTask(s.dbpool, s.syncId, s.taskId, s.packageName, s.packageVersion, s.startedAt, status, description)
		if err != nil {
			s.panic("error updating task: %v", err)
		}
	}
}

func (s *SideCar) sendState(state string) {
	err := db.UpsertState(s.dbpool, s.syncId, state, time.Now())
	if err != nil {
		s.panic("error updating state: %v", err)
	}
}
func (s *SideCar) bulkerEvent(connection, tableName string, payload any) error {
	v, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshalling event payload %v for %s: %v", payload, tableName, err)
	}
	body := bytes.NewReader(v)
	_, err = s.bulkerRequest(fmt.Sprintf("%s/post/%s?tableName=%s", s.bulkerURL, connection, url.QueryEscape(tableName)), body)
	if err != nil {
		return fmt.Errorf("error sending event to %s: %v", tableName, err)
	}
	return nil
}

func (s *SideCar) bulkerRequest(url string, payload io.Reader) ([]byte, error) {
	req, err := http.NewRequest("POST", url, payload)
	if err != nil {
		return nil, fmt.Errorf("error creating POST %s request: %v", url, err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", s.bulkerAuthToken))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s error: %v", url, err)
	}
	defer res.Body.Close()
	bd, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("POST %s read response error: %v", url, res.Status)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("POST %s error %v: %s", url, res.Status, string(bd))
	}
	return bd, nil
}

func (s *SideCar) loadState() (string, bool) {
	//load catalog from file /config/catalog.json and parse it
	statePath := "/config/state.json"
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return "", false
	}
	state, err := os.ReadFile(statePath)
	if err != nil {
		return "", false
	}
	st := string(state)
	if len(st) == 0 || st == "{}" {
		return "", false
	}
	s.initialState = st
	return st, true
}

func (s *SideCar) loadCatalog() error {
	//load catalog from file /config/catalog.json and parse it
	catalogPath := "/config/catalog.json"
	if _, err := os.Stat(catalogPath); os.IsNotExist(err) {
		return fmt.Errorf("catalog file %s doesn't exist", catalogPath)
	}
	catalogFile, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("error opening catalog file: %v", err)
	}
	catalog := Catalog{}
	err = json.Unmarshal(catalogFile, &catalog)
	if err != nil {
		return fmt.Errorf("error parsing catalog file: %v", err)
	}
	mp := make(map[string]*Stream, len(catalog.Streams))
	for _, stream := range catalog.Streams {
		mp[stream.Name] = stream
	}
	s.catalog = mp
	return nil
}

func joinStrings(str1, str2 string) string {
	if str1 == "" {
		return str2
	} else if str2 == "" {
		return str1
	}

	return str1 + ": " + str2
}
