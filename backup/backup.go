package backup

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/moby/runconfig"
)

type container struct {
	Config     runconfig.DecodeContainerConfig
	HostConfig runconfig.DecodeHostConfig
	Name       string            `json:"Name"`
	Volumes    map[string]string `json:"Volumes"`
}

type containerResponse struct {
	Id string `json:"ID"`
}

type ContainerBackup struct {
	rw    io.ReadWriteSeeker
	ts    time.Time
	addr  string
	proto string
}

func NewBackup(addr, proto string, rw io.ReadWriteSeeker) *ContainerBackup {
	backup := &ContainerBackup{
		addr:  addr,
		proto: proto,
		rw:    rw,
		ts:    time.Now(),
	}
	return backup
}

func (b *ContainerBackup) Store(containerId string) (uint, error) {
	container, _, err := b.getContainer(containerId)
	if err != nil {
		return 0, err
	}

	switch len(container.HostConfig.VolumesFrom) {
	case 0:
		return 0, errors.New("Couldn't find data container")
	case 1:
		break
	default:
		return 0, errors.New("Only containers with one data volume container are support right now")
	}

	return b.VolumeContainerStore(container.HostConfig.VolumesFrom[0])
}

func (b *ContainerBackup) VolumeContainerStore(containerId string) (uint, error) {
	volumeContainer, volumeContainerJson, err := b.getContainer(containerId)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(b.rw)
	th := &tar.Header{
		Name:       volumeContainerFilename,
		Size:       int64(len(volumeContainerJson)),
		ModTime:    b.ts,
		AccessTime: b.ts,
		ChangeTime: b.ts,
		Mode:       0644,
	}
	if err := tw.WriteHeader(th); err != nil {
		return 0, err
	}
	if _, err := tw.Write(volumeContainerJson); err != nil {
		return 0, err
	}

	n := uint(0)
	for path, hostPath := range volumeContainer.Volumes {
		volume := newContainerVolume(path, hostPath, tw)
		nl, err := volume.Store()
		if err != nil {
			return n, err
		}
		n = n + nl
	}
	return n, tw.Close()
}

func (b *ContainerBackup) Restore() error {
	tr := tar.NewReader(b.rw)
	oldContainerJson := []byte{}
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch th.Name {
		case volumeContainerFilename:
			var err error
			oldContainerJson, err = ioutil.ReadAll(tr)
			if err != nil {
				return err
			}
		}
	}
	if oldContainerJson == nil {
		return fmt.Errorf("Couldn't find volume container in backup")
	}

	oldContainer := &container{}
	if err := json.Unmarshal(oldContainerJson, oldContainer); err != nil {
		return err
	}

	config, err := json.Marshal(oldContainer.Config)
	if err != nil {
		return err
	}

	params := &url.Values{}
	params.Set("name", oldContainer.Name[1:]) // remove leading /
	resp, err := b.request("POST", fmt.Sprintf("/containers/create?%s", params.Encode()),
		bytes.NewReader(config))
	if err != nil {
		return err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	cr := &containerResponse{}
	if err := json.Unmarshal(body, cr); err != nil {
		return err
	}

	// we need to start the container once to create the volumes
	_, err = b.request("POST", fmt.Sprintf("/containers/%s/start", cr.Id), nil)
	if err != nil {
		return err
	}

	newContainer, _, err := b.getContainer(cr.Id)
	if err != nil {
		return err
	}

	trans := map[string]string{}
	// find new location for each volume found in old container
	for oldPath, oldHostPath := range oldContainer.Volumes {
		newHostPath := ""
		for path, hostPath := range newContainer.Volumes {
			if oldPath == path {
				newHostPath = hostPath
				break
			}
		}

		relOldHostPath := oldHostPath[len(filepath.Dir(oldHostPath))+1:]
		trans[relOldHostPath] = newHostPath
	}

	if _, err := b.rw.Seek(0, 0); err != nil {
		return err
	}
	tr = tar.NewReader(b.rw)

	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		path := strings.Split(th.Name, "/")
		if len(path) == 1 && th.Typeflag != tar.TypeDir { // ignore files right on root
			continue
		}
		destVolume := trans[path[0]]
		if destVolume == "" {
			fmt.Errorf("Couldn't find matching volume in new container")
		}

		path[0] = destVolume
		abs := filepath.Join(path...)
		if th.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(abs, 0755); err != nil {
				return err
			}
		} else {
			file, err := os.Create(abs)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, tr); err != nil {
				return err
			}
		}
		if err := os.Chown(abs, th.Uid, th.Gid); err != nil {
			return err
		}
		if err := os.Chmod(abs, os.FileMode(th.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func (b *ContainerBackup) request(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	conn, err := net.Dial(b.proto, b.addr)
	if err != nil {
		return nil, err
	}

	clientconn := httputil.NewClientConn(conn, nil)
	resp, err := clientconn.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("Error: %s", http.StatusText(resp.StatusCode))
		}

		return nil, fmt.Errorf("HTTP %s: %s", http.StatusText(resp.StatusCode), body)
	}
	return resp, nil
}

func (b *ContainerBackup) getContainer(containerId string) (*container, []byte, error) {
	resp, err := b.request("GET", fmt.Sprintf("/containers/%s/json", containerId), nil)
	if err != nil {
		return nil, nil, err
	}

	container := &container{}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, body, err
	}
	return container, body, json.Unmarshal(body, &container)
}
