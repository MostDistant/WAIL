//go:build linkstub

package main

// stubAudioEngine is the no-op AudioEngine used under -tags linkstub, where the
// cgo Link Audio binding is unavailable. It lets the app and logic tests build
// without the Link SDK; there is no real audio path in stub builds.
type stubAudioEngine struct{}

func newAudioEngine(_ *LinkBridge, _ string, _ func(waif []byte), _ int) AudioEngine {
	return &stubAudioEngine{}
}

func (s *stubAudioEngine) Start() error                                          { return nil }
func (s *stubAudioEngine) Stop()                                                 {}
func (s *stubAudioEngine) HandleRemoteAudio(_, _, _ string, _ []byte)            {}
func (s *stubAudioEngine) SetRoomAnchor(_ int64, _ float64, _ uint32, _ float64) {}
func (s *stubAudioEngine) RoomIndex(_ int64) (int64, bool)                       { return 0, false }
func (s *stubAudioEngine) CaptureChannels() []CaptureChannelInfo                 { return nil }
func (s *stubAudioEngine) SetCaptureEnabled(_ string, _ bool)                    {}
