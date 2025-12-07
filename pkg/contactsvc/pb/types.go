package pb

type Service int32

const (
	SERVICE_UNSPECIFIED Service = 0
	TELEGRAM            Service = 1
	WHATSAPP            Service = 2
)

type Contact struct {
	Id        int64
	FirstName string
	LastName  string
	Username  string
	Phone     string
	Service   Service
}

func (*Contact) ProtoMessage()  {}
func (*Contact) Reset()         {}
func (*Contact) String() string { return "Contact" }

type Channel struct {
	Id       int64
	Title    string
	Username string
	Service  Service
}

func (*Channel) ProtoMessage()  {}
func (*Channel) Reset()         {}
func (*Channel) String() string { return "Channel" }

type Group struct {
	Id      int64
	Title   string
	Service Service
}

func (*Group) ProtoMessage()  {}
func (*Group) Reset()         {}
func (*Group) String() string { return "Group" }

type Supergroup struct {
	Id       int64
	Title    string
	Username string
	Service  Service
}

func (*Supergroup) ProtoMessage()  {}
func (*Supergroup) Reset()         {}
func (*Supergroup) String() string { return "Supergroup" }

type FinalResult struct {
	Humans      []*Contact    `json:"humans"`
	Bots        []*Contact    `json:"bots"`
	Channels    []*Channel    `json:"channels"`
	Groups      []*Group      `json:"groups"`
	Supergroups []*Supergroup `json:"supergroups"`
}

func (*FinalResult) ProtoMessage()  {}
func (*FinalResult) Reset()         {}
func (*FinalResult) String() string { return "FinalResult" }
func (x *FinalResult) GetHumans() []*Contact {
	if x != nil {
		return x.Humans
	}
	return nil
}
func (x *FinalResult) GetBots() []*Contact {
	if x != nil {
		return x.Bots
	}
	return nil
}
func (x *FinalResult) GetChannels() []*Channel {
	if x != nil {
		return x.Channels
	}
	return nil
}
func (x *FinalResult) GetGroups() []*Group {
	if x != nil {
		return x.Groups
	}
	return nil
}
func (x *FinalResult) GetSupergroups() []*Supergroup {
	if x != nil {
		return x.Supergroups
	}
	return nil
}

type Empty struct{}

func (*Empty) ProtoMessage()  {}
func (*Empty) Reset()         {}
func (*Empty) String() string { return "Empty" }
