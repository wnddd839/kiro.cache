package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	flushEvery = 250 * time.Millisecond
	pruneEvery = time.Minute
	maxLine    = 1 << 20 // 单行上限；超长视为损坏
)

// Open 打开（或新建）path 处的账本并启动后台落盘。path 为空时只在内存中。
//
// 启动时重放文件：跳过损坏行、丢弃保留期之前的记录；有所丢弃或文件超过保留内容两倍时原子重写。
func Open(path string, opts Options) (*Journal, error) {
	j := newJournal(path, opts)
	if path != "" {
		if err := j.load(); err != nil {
			return nil, err
		}
	}
	go j.loop()
	return j, nil
}

func (j *Journal) load() error {
	f, err := os.Open(j.path)
	if errors.Is(err, fs.ErrNotExist) {
		return os.MkdirAll(filepath.Dir(j.path), 0o700)
	}
	if err != nil {
		return err
	}
	defer f.Close()

	cutoff := j.now().Add(-j.retention)
	var (
		kept     bytes.Buffer
		size     int64
		dropped  bool
		lastByte byte = '\n'
	)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// 超长行：读完整行后按长度判断
			buf := append([]byte(nil), line...)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				if len(buf) <= maxLine {
					buf = append(buf, line...)
				}
			}
			line = buf
		}
		if len(line) > 0 {
			size += int64(len(line))
			lastByte = line[len(line)-1]
			if j.replay(line, cutoff) {
				kept.Write(bytes.TrimRight(line, "\r\n"))
				kept.WriteByte('\n')
			} else if len(bytes.TrimSpace(line)) > 0 {
				dropped = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	f.Close()

	// 末行缺换行（写到一半崩溃）也重写，否则后续追加会拼到同一行
	if dropped || lastByte != '\n' || size > 2*int64(kept.Len()) {
		return writeFileAtomic(j.path, kept.Bytes())
	}
	return nil
}

// replay 解析一行并计入内存；损坏或过期返回 false。
func (j *Journal) replay(line []byte, cutoff time.Time) bool {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || len(line) > maxLine {
		return false
	}
	var e Entry
	if json.Unmarshal(line, &e) != nil || e.Time.IsZero() || e.Time.Before(cutoff) {
		return false
	}
	e.Credits = finite(e.Credits)
	e.CostUSD = finite(e.CostUSD)
	j.mu.Lock()
	j.addLocked(e)
	j.mu.Unlock()
	return true
}

// Flush 把待写记录追加到文件。写失败的数据留在缓冲里下次重试。
func (j *Journal) Flush() error {
	if j.path == "" {
		return nil
	}
	j.ioMu.Lock()
	defer j.ioMu.Unlock()

	j.mu.Lock()
	buf := j.pending
	j.pending = nil
	j.mu.Unlock()
	if len(buf) == 0 {
		return nil
	}

	err := appendFile(j.path, buf)
	if err != nil {
		// 放回队首，保持顺序
		j.mu.Lock()
		if len(buf)+len(j.pending) <= maxPending {
			j.pending = append(buf, j.pending...)
		}
		j.mu.Unlock()
	}
	return err
}

func appendFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

// Close 停止后台并写完待写记录。可重复调用。
func (j *Journal) Close() error {
	j.closeOnce.Do(func() {
		close(j.stop)
		<-j.done
		j.closeErr = j.Flush()
	})
	return j.closeErr
}

func (j *Journal) loop() {
	defer close(j.done)
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	lastPrune := time.Now()
	for {
		select {
		case <-j.stop:
			return
		case <-tick.C:
			_ = j.Flush()
			if time.Since(lastPrune) >= pruneEvery {
				lastPrune = time.Now()
				j.prune()
			}
		}
	}
}

// writeFileAtomic 写临时文件再 rename，避免写一半崩溃损坏账本。
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 成功后无事可做
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, path)
}
