package core

import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "strconv"
 "time"

 "github.com/rs/zerolog"
 _ "github.com/mattn/go-sqlite3"
 "go.mau.fi/mautrix-meta/pkg/messagix"
 "go.mau.fi/mautrix-meta/pkg/messagix/cookies"
 "go.mau.fi/mautrix-meta/pkg/messagix/table"
 "go.mau.fi/mautrix-meta/pkg/messagix/types"
 "go.mau.fi/whatsmeow/store"
 "go.mau.fi/whatsmeow/store/sqlstore"
 waTypes "go.mau.fi/whatsmeow/types"
 "go.mau.fi/whatsmeow/types/events"
)

type Event map[string]any
type Callback func(Event)

func ParseCookies(data []byte, platform string) (*cookies.Cookies, error) {
 p := types.PlatformFromString(platform)
 if p != types.Messenger && p != types.Facebook { return nil, fmt.Errorf("platform must be messenger or facebook") }
 var values map[string]string
 if err := json.Unmarshal(data, &values); err != nil {
  var list []struct { Name string `json:"name"`; Value string `json:"value"` }
  if err := json.Unmarshal(data, &list); err != nil { return nil, fmt.Errorf("invalid cookies JSON: %w", err) }
  values = make(map[string]string); for _, item := range list { values[item.Name] = item.Value }
 }
 c := &cookies.Cookies{Platform:p}; mapped := make(map[cookies.MetaCookieName]string)
 for k,v := range values { mapped[cookies.MetaCookieName(k)] = v }; c.UpdateValues(mapped)
 if missing := c.GetMissingCookieNames(); len(missing)>0 { return nil, fmt.Errorf("missing cookies: %v", missing) }
 if c.GetUserID() == 0 { return nil, fmt.Errorf("invalid c_user cookie") }; return c,nil
}

func Run(ctx context.Context, c *cookies.Cookies, dataDir string, emit Callback) error {
 state := func(s string) { emit(Event{"type":"state", "state":s}) }
 log := zerolog.Nop(); cli := messagix.NewClient(c, log, &messagix.Config{})
 cli.SetEventHandler(func(_ context.Context, raw any) {
  switch e := raw.(type) {
  case *messagix.ConnectedEvent: state("dgw_connected")
  case *messagix.ReconnectedEvent: state("dgw_reconnected")
  case *messagix.TransientDisconnectEvent: emit(Event{"type":"error","error":fmt.Sprint("dgw disconnected: ",e.Err)})
  case *messagix.PermanentErrorEvent: emit(Event{"type":"error","error":fmt.Sprint("dgw: ",e.Err)})
  case *table.LSTable:
   for _, m := range e.LSInsertMessage { text:=m.Text; if text=="" {text="[attachment]"}; emit(Event{"type":"message","time":time.UnixMilli(m.TimestampMs).Format(time.RFC3339),"thread_id":strconv.FormatInt(m.ThreadKey,10),"e2ee":false,"sender":strconv.FormatInt(m.SenderId,10),"text":text}) }
  }
 })
 state("loading_inbox"); user, tbl, err := cli.LoadMessagesPage(ctx); if err != nil { return fmt.Errorf("load inbox: %w",err) }
 if user.GetFBID()!=c.GetUserID() { return fmt.Errorf("inbox account does not match c_user") }
 threads := map[int64]bool{}; for _, t := range tbl.LSDeleteThenInsertThread { threads[t.ThreadKey]=true }; for _, t := range tbl.LSUpdateOrInsertThread { threads[t.ThreadKey]=true }
 emit(Event{"type":"state","state":"inbox_loaded","threads":len(threads)})
 if err = os.MkdirAll(dataDir,0700); err != nil { return fmt.Errorf("create data directory: %w",err) }
 db,err:=sqlstore.New(ctx,"sqlite3", "file:"+filepath.Join(dataDir,"e2ee.db")+"?_foreign_keys=on",nil); if err!=nil{return fmt.Errorf("open e2ee store: %w",err)}; defer db.Close()
 devices,err:=db.GetAllDevices(ctx); if err!=nil{return fmt.Errorf("load devices: %w",err)}
 var dev *store.Device; for _, d:=range devices { if d.ID != nil && d.ID.User==strconv.FormatInt(c.GetUserID(),10) && d.ID.Server==waTypes.MessengerServer { if dev!=nil{return fmt.Errorf("multiple devices for account in e2ee.db")}; dev=d } }
 // Device registration follows pkg/connector/client.go:562-605.
 fresh:=dev==nil; if fresh {dev=db.NewDevice(); if suggested:=cli.MessengerLite.GetSuggestedDeviceID(); suggested.String()!="00000000-0000-0000-0000-000000000000" { dev.FacebookUUID=suggested }}
 cli.SetDevice(dev); if fresh { state("registering_e2ee"); if err=cli.RegisterE2EE(ctx,c.GetUserID()); err!=nil{return fmt.Errorf("register e2ee: %w",err)}; if err=dev.Save(ctx); err!=nil{return fmt.Errorf("save e2ee device: %w",err)} }
 wa,err:=cli.PrepareE2EEClient(); if err!=nil{return fmt.Errorf("prepare e2ee: %w",err)}
 wa.AddEventHandler(func(raw any) { switch e:=raw.(type) {
 case *events.Connected: state("e2ee_connected")
 case *events.Disconnected: state("e2ee_disconnected")
 case *events.UndecryptableMessage: emit(Event{"type":"error","error":"undecryptable e2ee message"})
 case *events.FBMessage:
  content:=e.GetConsumerApplication().GetPayload().GetContent(); if content==nil{return}; text:=content.GetMessageText().GetText(); if text=="" {text=content.GetExtendedTextMessage().GetText().GetText()}; if text=="" {text="[attachment]"}; emit(Event{"type":"message","time":e.Info.Timestamp.Format(time.RFC3339),"thread_id":e.Info.Chat.String(),"e2ee":true,"sender":e.Info.Sender.String(),"text":text})
 } })
 state("connecting_dgw"); if err=cli.Connect(ctx); err!=nil{return fmt.Errorf("connect dgw: %w",err)}; defer cli.Disconnect()
 state("connecting_e2ee"); if err=wa.Connect(); err!=nil{return fmt.Errorf("connect e2ee: %w",err)}; defer wa.Disconnect()
 <-ctx.Done(); state("stopped"); return nil
}
