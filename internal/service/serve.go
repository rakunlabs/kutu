package service

import "context"

// metaServeSettings is the kutu_meta key holding the singleton
// ServeSettings document (FTP/SFTP/TFTP/WebDAV + shared users/shares).
const metaServeSettings = "serve_settings"

// GetServeSettings returns the persisted file-serving configuration. A
// fresh install (no row yet) yields a zero-value ServeSettings with all
// protocols disabled, which is the safe default.
func (s *Service) GetServeSettings(ctx context.Context) (*ServeSettings, error) {
	var cfg ServeSettings
	if _, err := s.store.GetMeta(ctx, metaServeSettings, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SetServeSettings persists the file-serving configuration. The caller
// (api layer) is responsible for reconciling the live servers afterwards.
func (s *Service) SetServeSettings(ctx context.Context, cfg *ServeSettings) error {
	if cfg == nil {
		cfg = &ServeSettings{}
	}
	return s.store.SetMeta(ctx, metaServeSettings, cfg)
}
