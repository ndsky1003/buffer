package buffer

func Options() *Option {
	return &Option{}
}

type Option struct {
	CalibrateCalls *uint64
	MaxPercentile  *float64
	MinSize        *uint64
	MaxSize        *uint64
	CalibratedSz   *uint64
}

func (o *Option) SetCalibrateCalls(v uint64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.CalibrateCalls = &v
	return o
}
func (o *Option) SetMaxPercentile(v float64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.MaxPercentile = &v
	return o
}
func (o *Option) SetMinSize(v uint64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.MinSize = &v
	return o
}
func (o *Option) SetMaxSize(v uint64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.MaxSize = &v
	return o
}

func (o *Option) SetCalibratedSz(v uint64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.CalibratedSz = &v
	return o
}

func (o *Option) merge(delta *Option) {
	if delta == nil || o == nil {
		return
	}
	if delta.CalibrateCalls != nil {
		o.CalibrateCalls = delta.CalibrateCalls
	}
	if delta.MaxPercentile != nil {
		o.MaxPercentile = delta.MaxPercentile
	}
	if delta.MinSize != nil {
		o.MinSize = delta.MinSize
	}
	if delta.MaxSize != nil {
		o.MaxSize = delta.MaxSize
	}
	if delta.CalibratedSz != nil {
		o.CalibratedSz = delta.CalibratedSz
	}
}

func (o Option) Merge(opts ...*Option) Option {
	for _, opt := range opts {
		o.merge(opt)
	}
	return o
}
