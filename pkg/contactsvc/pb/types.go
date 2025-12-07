package pb

type Service int32

const (
	SERVICE_UNSPECIFIED Service = 0
	TELEGRAM            Service = 1
	WHATSAPP            Service = 2
)

type Contact struct {
	Id        int64  `protobuf:"varint,1,opt,name=id,proto3" json:"id"`
	FirstName string `protobuf:"bytes,2,opt,name=first_name,json=firstName,proto3" json:"first_name"`
	LastName  string `protobuf:"bytes,3,opt,name=last_name,json=lastName,proto3" json:"last_name"`
	Username  string `protobuf:"bytes,4,opt,name=username,proto3" json:"username"`
	Phone     string `protobuf:"bytes,5,opt,name=phone,proto3" json:"phone"`
}

func (*Contact) ProtoMessage()  {}
func (*Contact) Reset()         {}
func (*Contact) String() string { return "Contact" }

type Channel struct {
	Id       int64  `protobuf:"varint,1,opt,name=id,proto3" json:"id"`
	Title    string `protobuf:"bytes,2,opt,name=title,proto3" json:"title"`
	Username string `protobuf:"bytes,3,opt,name=username,proto3" json:"username"`
}

func (*Channel) ProtoMessage()  {}
func (*Channel) Reset()         {}
func (*Channel) String() string { return "Channel" }

type Group struct {
	Id    int64  `protobuf:"varint,1,opt,name=id,proto3" json:"id"`
	Title string `protobuf:"bytes,2,opt,name=title,proto3" json:"title"`
}

func (*Group) ProtoMessage()  {}
func (*Group) Reset()         {}
func (*Group) String() string { return "Group" }

type Supergroup struct {
	Id       int64  `protobuf:"varint,1,opt,name=id,proto3" json:"id"`
	Title    string `protobuf:"bytes,2,opt,name=title,proto3" json:"title"`
	Username string `protobuf:"bytes,3,opt,name=username,proto3" json:"username"`
}

func (*Supergroup) ProtoMessage()  {}
func (*Supergroup) Reset()         {}
func (*Supergroup) String() string { return "Supergroup" }

type Result struct {
	Humans      []*Contact    `protobuf:"bytes,1,rep,name=humans,proto3" json:"humans"`
	Bots        []*Contact    `protobuf:"bytes,2,rep,name=bots,proto3" json:"bots"`
	Channels    []*Channel    `protobuf:"bytes,3,rep,name=channels,proto3" json:"channels"`
	Groups      []*Group      `protobuf:"bytes,4,rep,name=groups,proto3" json:"groups"`
	Supergroups []*Supergroup `protobuf:"bytes,5,rep,name=supergroups,proto3" json:"supergroups"`
	Service     Service       `protobuf:"varint,6,opt,name=service,proto3,enum=contacts.Service" json:"service"`
	UserId      uint32        `protobuf:"varint,7,opt,name=user_id,json=userId,proto3" json:"user_id"`
}

func (*Result) ProtoMessage()  {}
func (*Result) Reset()         {}
func (*Result) String() string { return "Result" }
func (x *Result) GetHumans() []*Contact {
	if x != nil {
		return x.Humans
	}
	return nil
}
func (x *Result) GetBots() []*Contact {
	if x != nil {
		return x.Bots
	}
	return nil
}
func (x *Result) GetChannels() []*Channel {
	if x != nil {
		return x.Channels
	}
	return nil
}
func (x *Result) GetGroups() []*Group {
	if x != nil {
		return x.Groups
	}
	return nil
}
func (x *Result) GetSupergroups() []*Supergroup {
	if x != nil {
		return x.Supergroups
	}
	return nil
}
func (x *Result) GetService() Service {
	if x != nil {
		return x.Service
	}
	return SERVICE_UNSPECIFIED
}
func (x *Result) GetUserId() uint32 {
	if x != nil {
		return x.UserId
	}
	return 0
}

type Empty struct{}

func (*Empty) ProtoMessage()  {}
func (*Empty) Reset()         {}
func (*Empty) String() string { return "Empty" }
