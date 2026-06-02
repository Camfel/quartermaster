package cri

import (
	"context"
	"fmt"
	"sync"

	"quartermaster/pkg/types"
)

// MockContainer represents a mock container.
type MockContainer struct {
	ID    string
	Name  string
	Image string
}

// MockContainerClient is a mock implementation of ContainerClient for testing.
type MockContainerClient struct {
	mu         sync.Mutex
	containers map[string]*MockContainer

	// OnPullImage is called when PullImage is invoked.
	OnPullImage func(ref string) (string, error)
	// OnCreateContainer is called when CreateContainer is invoked.
	OnCreateContainer func(svc types.Service) (string, error)
	// OnStartContainer is called when StartContainer is invoked.
	OnStartContainer func(containerID string) error
	// OnStopContainer is called when StopContainer is invoked.
	OnStopContainer func(containerID string) error
	// OnDeleteContainer is called when DeleteContainer is invoked.
	OnDeleteContainer func(containerID string) error
	// OnListContainers is called when ListContainers is invoked.
	OnListContainers func() ([]ContainerInfo, error)
}

// NewMockContainerClient initializes a new MockContainerClient.
func NewMockContainerClient() *MockContainerClient {
	return &MockContainerClient{
		containers: make(map[string]*MockContainer),
	}
}

// PullImage implements the ContainerClient interface.
func (m *MockContainerClient) PullImage(ctx context.Context, ref string) (string, error) {
	if m.OnPullImage != nil {
		return m.OnPullImage(ref)
	}
	return ref, nil
}

// CreateContainer implements the ContainerClient interface.
func (m *MockContainerClient) CreateContainer(ctx context.Context, svc types.Service) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnCreateContainer != nil {
		id, err := m.OnCreateContainer(svc)
		if err != nil {
			return "", err
		}
		m.containers[id] = &MockContainer{ID: id, Name: svc.Name, Image: svc.Image}
		return id, nil
	}

	id := svc.Name
	m.containers[id] = &MockContainer{ID: id, Name: svc.Name, Image: svc.Image}
	return id, nil
}

// StartContainer implements the ContainerClient interface.
func (m *MockContainerClient) StartContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnStartContainer != nil {
		return m.OnStartContainer(containerID)
	}

	if _, ok := m.containers[containerID]; !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	return nil
}

// StopContainer implements the ContainerClient interface.
func (m *MockContainerClient) StopContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnStopContainer != nil {
		return m.OnStopContainer(containerID)
	}

	if _, ok := m.containers[containerID]; !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	return nil
}

// DeleteContainer implements the ContainerClient interface.
func (m *MockContainerClient) DeleteContainer(ctx context.Context, containerID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnDeleteContainer != nil {
		return m.OnDeleteContainer(containerID)
	}

	if _, ok := m.containers[containerID]; !ok {
		return fmt.Errorf("container %s not found", containerID)
	}
	delete(m.containers, containerID)
	return nil
}

// ListContainers implements the ContainerClient interface.
func (m *MockContainerClient) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.OnListContainers != nil {
		return m.OnListContainers()
	}

	var infos []ContainerInfo
	for _, c := range m.containers {
		infos = append(infos, ContainerInfo{
			ID:    c.ID,
			Name:  c.Name,
			Image: c.Image,
		})
	}
	return infos, nil
}

// GetContainerPID implements the ContainerClient interface.
func (m *MockContainerClient) GetContainerPID(ctx context.Context, containerID string) (uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.containers[containerID]; !ok {
		return 0, fmt.Errorf("container %s not found", containerID)
	}
	// Return a mock PID (non-zero means running)
	return 1000, nil
}
