package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type JsonAny = interface{}
type JsonTable = map[string]JsonAny
type JsonArray = []JsonAny

type TgApiError string

type CommandHandler = func(message JsonTable)

func (e TgApiError) Error() string {
	return string(e)
}

type MemberRecord struct {
	Name    string
	License string
}

type EventInfo struct {
	Description string
	EventId     int

	Registrations []MemberRecord
}

const (
	//	tg_api_url    = "https://api.telegram.org/bot"
	tg_api_url    = "http://83.220.168.42:8090/bot"
	update_freq   = 0.5 // hz
	updates_limit = 10
)

const (
	AuthorizeErrorMsg       = "Вы должны обладать правами администратора для выполнения данной команды."
	EventOpenAskDescription = "Введите описание планируемого события:"
	EventOpenAlreadyExists  = "В выбранном канале уже есть активное событие. Закройте его для создания нового."
	EventOpenReport         = "Событие #%d созданно."
	ReplyTimoutMsg          = "Срок ожидания ответа истёк. Попробуйте выполнить операцию ещё раз."
)

const HelpMsg = `
	/open - Создать событие (только для админов канала)
	/close - Закрыть региcтрацию на событие (только для админов канала)
	/show - Показать текущее событие и список зарегестрированных участников
	/history - Показать историю проводимых событий
	/register - Зарегестрировать участника на текущее событие
	/unregister - Отменить регистрацию
`

var (
	http_client *http.Client
	bot_url     string
	bot_name    string

	id_counter     int32
	current_events = map[json.Number]*EventInfo{}
)

func toJson(obj JsonAny) string {
	if obj == nil {
		return "{}"
	}
	if str, err := json.Marshal(obj); err == nil {
		var out bytes.Buffer
		json.Indent(&out, str, "", "  ")
		return string(out.Bytes())
	}
	return "{}"
}

func hasKey(v JsonTable, key string) bool {
	_, ok := v[key]
	return ok
}

func getStr(v JsonTable, key string) string {
	q, _ := v[key].(string)
	return q
}

func getNum(v JsonTable, key string) json.Number {
	q, _ := v[key].(json.Number)
	return q
}

func getInt(v JsonTable, key string) int64 {
	q, _ := getNum(v, key).Int64()
	return q
}

func getTbl(v JsonTable, key string) JsonTable {
	q, _ := v[key].(JsonTable)
	return q
}

func tgApiCall(tg_func string, msg JsonTable) (JsonAny, error) {
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	log.Printf("Call API func %v\n%s", tg_func, toJson(msg))
	resp, err := http_client.Post(bot_url+tg_func, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	var respJson JsonAny
	d := json.NewDecoder(resp.Body)
	d.UseNumber()
	if err = d.Decode(&respJson); err != nil {
		return nil, err
	}

	resp_tbl, ok := respJson.(JsonTable)
	if ok != true {
		return nil, TgApiError("non-table response")
	}

	ok, hasOk := resp_tbl["ok"].(bool)
	if hasOk == false {
		return nil, TgApiError("Bad response status")
	}

	if ok != true {
		return nil, TgApiError(getStr(resp_tbl, "description"))
	}

	return resp_tbl["result"], err
}

func pollMessages(offset int64) []JsonTable {
	var result []JsonTable
	resp, err := tgApiCall("getUpdates",
		JsonTable{
			"offset":  offset,
			"limit":   updates_limit,
			"timeout": 15,
		})

	if err != nil {
		log.Printf("Failed to fetch updates %v\n", err)
		return result
	}

	for _, message := range resp.(JsonArray) {
		result = append(result, message.(JsonTable))
	}
	return result
}

func sendReply(chat_id interface{}, message_id json.Number, text string) {
	_, err := tgApiCall("sendMessage",
		JsonTable{
			"chat_id":             chat_id,
			"reply_to_message_id": message_id,
			"text":                text,
			"parse_mode":          "Markdown",
		})
	if err != nil {
		log.Printf("failed to send reply %v", err)
	}
}

func sendPrivateMessage(chat_id interface{}, text string, force_reply bool) (JsonAny, error) {
	request := JsonTable{
		"chat_id":    chat_id,
		"text":       text,
		"parse_mode": "Markdown",
	}

	if force_reply {
		request["reply_markup"] = JsonTable{
			"force_reply": true,
		}
	}

	resp, err := tgApiCall("sendMessage", request)
	if err != nil {
		log.Printf("failed to send reply %v", err)
	}
	return resp, err
}

func isUserAdmin(user_id json.Number, chat_id json.Number) (bool, error) {
	resp, err := tgApiCall("getChatMember",
		JsonTable{
			"chat_id": chat_id,
			"user_id": user_id,
		})

	if err != nil {
		return false, err
	}

	status := getStr(resp.(JsonTable), "status")
	if status == "creator" || status == "administrator" {
		return true, nil
	}
	return false, nil
}

func getChatId(message JsonTable) json.Number {
	return getNum(getTbl(message, "chat"), "id")
}

func getSenderId(message JsonTable) json.Number {
	return getNum(getTbl(message, "from"), "id")
}

func authorize(message JsonTable) bool {
	chat_id := getChatId(message)
	user_id := getSenderId(message)
	auth_ok, _ := isUserAdmin(user_id, chat_id)
	if auth_ok {
		return true
	}

	sendPrivateMessage(user_id, AuthorizeErrorMsg, false)
	return false
}

var reply_hub = map[json.Number]chan JsonAny{}
var reply_hub_mux = sync.Mutex{}

func waitForReply(message_id json.Number) (JsonAny, error) {
	reply_hub_mux.Lock()
	ch, ok := reply_hub[message_id]
	if ok == false {
		ch = make(chan JsonAny)
		reply_hub[message_id] = ch
	}
	reply_hub_mux.Unlock()

	select {
	case message := <-ch:
		reply_hub_mux.Lock()
		delete(reply_hub, message_id)
		reply_hub_mux.Unlock()
		return message, nil
	case <-time.After(5 * time.Minute):
		reply_hub_mux.Lock()
		delete(reply_hub, message_id)
		reply_hub_mux.Unlock()
		return nil, TgApiError("Reply timeout")
	}

	return nil, nil
}

func processReply(message JsonTable) {
	reply_message_id := getNum(getTbl(message, "reply_to_message"), "message_id")
	reply_hub_mux.Lock()
	ch, ok := reply_hub[reply_message_id]
	reply_hub_mux.Unlock()
	if ok {
		ch <- message
	} else {
		sendPrivateMessage(getSenderId(message), ReplyTimoutMsg, false)
	}
}

func askQuestion(userId json.Number, question string) (JsonAny, error) {
	resp, err := sendPrivateMessage(userId, question, true)
	if err != nil {
		return nil, err
	}

	message_id := getNum(resp.(JsonTable), "message_id")
	return waitForReply(message_id)
}

func eventOpen(message JsonTable) {
	if !authorize(message) {
		return
	}
	chat_id := getChatId(message)
	user_id := getSenderId(message)

	_, ok := current_events[chat_id]
	if ok {
		sendPrivateMessage(user_id, EventOpenAlreadyExists, false)
		return
	}

	answer, err := askQuestion(user_id, EventOpenAskDescription)
	if err != nil {
		log.Printf("Failed to get answer: %v", err)
		return
	}

	desc := getStr(answer.(JsonTable), "text")
	log.Printf("eventOpen reply: %v", desc)
	newEvent := EventInfo{}
	newEvent.Description = desc
	newEvent.EventId = int(atomic.AddInt32(&id_counter, 1))
	current_events[chat_id] = &newEvent

	sendPrivateMessage(user_id, fmt.Sprintf(EventOpenReport, newEvent.EventId), false)
}

func eventClose(message JsonTable) {
	if !authorize(message) {
		return
	}

	//chat_id := getChatId(message)
	user_id := getSenderId(message)

	replyKeyboardMarkup := JsonTable{
		"keyboard":        [][]string{{"YES"}, {"NO"}},
		"resize_keyboard": true,
		"selective":       true,
		"force_reply":     true,
	}

	request := JsonTable{
		"chat_id":      user_id,
		"text":         "Yes/No",
		"parse_mode":   "Markdown",
		"reply_markup": replyKeyboardMarkup,
	}

	resp, err := tgApiCall("sendMessage", request)
	if err != nil {
		log.Printf("failed to send reply %v", err)
	}

	log.Printf("<%s", toJson(resp))
	message_id := getNum(resp.(JsonTable), "message_id")
	log.Printf("wait for reply to %d", message_id)
	reply, err := waitForReply(message_id)

	log.Printf("got reply to message %d", message_id)
	log.Println(toJson(reply))

	request = JsonTable{
		"chat_id": user_id,
		"text":    "remove_keyboard",
		"reply_markup": JsonTable{
			"remove_keyboard": true,
		},
	}
	resp, err = tgApiCall("sendMessage", request)
	if err != nil {
		log.Println(err)
	}

	/*
		_, ok := current_events[chat_id]
		if !ok {
			sendPrivateMessage(user_id, EventOpenAlreadyExists, false)
			return
		}
	*/
}

func eventShow(message JsonTable) {
}

func history(message JsonTable) {
}

func register(message JsonTable) {
}

func unregister(message JsonTable) {
}

func help(message JsonTable) {
	sendPrivateMessage(getSenderId(message), HelpMsg, false)
}

func whoAmI(message JsonTable) {
	chat_id := getNum(getTbl(message, "chat"), "id")
	message_id := getNum(message, "message_id")
	resp, err := tgApiCall("getChatMember",
		JsonTable{
			"chat_id": chat_id,
			"user_id": getNum(getTbl(message, "from"), "id"),
		})
	if err == nil {
		sendReply(chat_id, message_id, "```\n"+toJson(resp)+"```")
	} else {
		log.Printf("failed to get chat member %v", err)
	}
}

var commandHandlers = map[string]CommandHandler{
	"/open":       eventOpen,
	"/close":      eventClose,
	"/history":    history,
	"/show":       eventShow,
	"/register":   register,
	"/unregister": register,
	"/whoami":     whoAmI,
	"/help":       help,
}

func handleMessage(messageObj JsonTable) {
	message := getTbl(messageObj, "message")
	if hasKey(message, "reply_to_message") {
		processReply(message)
	} else if hasKey(message, "chat") {
		log.Printf(toJson(messageObj))
		text := getStr(message, "text")

		i := strings.Index(text, "@")
		if i != -1 {
			botName := text[i+1:]
			if botName != bot_name {
				return
			}
			text = text[:i]
		}
		log.Printf("text %s", text)

		handler, ok := commandHandlers[text]
		if ok {
			handler(message)
		}
	}
}

func main() {
	bot_token := os.Getenv("BOT_TOKEN")
	bot_url = tg_api_url + bot_token + "/"
	log.Printf("Bot url is %s", bot_url)
	http_client = &http.Client{}

	me, err := tgApiCall("getMe", JsonTable{})
	if err != nil {
		log.Fatalf("Failed to get bot info: %v", err)
	}
	log.Print(toJson(me))
	bot_name = getStr(me.(JsonTable), "username")

	updatesOffset := int64(0)
	for {
		for _, message := range pollMessages(updatesOffset) {
			go handleMessage(message)
			updatesOffset = getInt(message, "update_id") + 1
		}
		time.Sleep((1000 / update_freq) * time.Millisecond)
	}
}
