package buffer

func Options() *Option {
	return &Option{}
}

type Option struct {
	CalibratePeriod *uint64  //校准周期
	MaxPercent      *float64 //相当于一个门卫,当cap超过CalibratedSz 就交给gc释放
	MinSize         *uint64  //最小尺寸
	MaxSize         *uint64  //最大尺寸
	CalibratedSz    *uint64  //当前初始的校准尺寸
}

func (o *Option) SetCalibratePeriod(v uint64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.CalibratePeriod = &v
	return o
}
func (o *Option) SetMaxPercent(v float64) *Option {
	if o == nil {
		o = &Option{}
	}
	o.MaxPercent = &v
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
	if delta.CalibratePeriod != nil {
		o.CalibratePeriod = delta.CalibratePeriod
	}
	if delta.MaxPercent != nil {
		o.MaxPercent = delta.MaxPercent
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
