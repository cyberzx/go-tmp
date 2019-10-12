package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

/*
	#admin commands

	/event-open
	/event-close
	/history

	/event-show
	/register
	/unregister

*/

type JsonAny = interface{}
type JsonTable = map[string]JsonAny
type JsonArray = []JsonAny

type TgApiError string

type CommandHandler = func(message JsonTable)

func (e TgApiError) Error() string {
	return string(e)
}

const (
	tg_api_url    = "https://api.telegram.org/bot"
	bot_token     = ""
	update_freq   = 0.5 // hz
	updates_limit = 10
)

const (
	AUTHORIZE_ERROR_TXT = "Вы должны обладать правами администратора для выполнения данной команды."
	EVENT_OPEN_QUESTION = "Введите описание планируемого события:"
	REPLY_TIMEOUT       = "Срок ожидания ответа истёк. Попробуйте выполнить операцию ещё раз."
)

const HELP_MESSAGE = `
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

	respTbl, ok := respJson.(JsonTable)
	if ok != true {
		return nil, TgApiError("non-table response")
	}

	ok, hasOk := respTbl["ok"].(bool)
	if hasOk == false {
		return nil, TgApiError("Bad response status")
	}

	if ok != true {
		return nil, TgApiError(getStr(respTbl, "description"))
	}

	return respTbl["result"], err
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

func authorize(message JsonTable) bool {
	chatId := getNum(getTbl(message, "chat"), "id")
	authOk, _ := isUserAdmin(getNum(getTbl(message, "from"), "id"), chatId)
	if authOk {
		return true
	}

	sendPrivateMessage(getNum(getTbl(message, "from"), "id"), AUTHORIZE_ERROR_TXT, false)
	return false
}

var replyHub = map[json.Number]chan JsonAny{}
var replyHubMux = sync.Mutex{}

func waitForReply(message_id json.Number) (JsonAny, error) {
	replyHubMux.Lock()
	ch, ok := replyHub[message_id]
	if ok == false {
		ch = make(chan JsonAny)
		replyHub[message_id] = ch
	}
	replyHubMux.Unlock()

	select {
	case message := <-ch:
		replyHubMux.Lock()
		delete(replyHub, message_id)
		replyHubMux.Unlock()
		return message, nil
	case <-time.After(5 * time.Minute):
		replyHubMux.Lock()
		delete(replyHub, message_id)
		replyHubMux.Unlock()
		return nil, TgApiError("Reply timeout")
	}

	return nil, nil
}

func processReply(message JsonTable) {
	reply_message_id := getNum(getTbl(message, "reply_to_message"), "message_id")
	replyHubMux.Lock()
	ch, ok := replyHub[reply_message_id]
	replyHubMux.Unlock()
	if ok {
		ch <- message
	} else {
		sendPrivateMessage(getNum(getTbl(message, "from"), "id"), REPLY_TIMEOUT, false)
	}
}

func askQuestion(userId json.Number, question string) (JsonAny, error) {
	resp, err := sendPrivateMessage(userId, question, true)
	if err != nil {
		return nil, err
	}

	messageId := getNum(resp.(JsonTable), "message_id")
	return waitForReply(messageId)
}

func eventOpen(message JsonTable) {
	if !authorize(message) {
		return
	}

	userId := getNum(getTbl(message, "from"), "id")
	answer, err := askQuestion(userId, EVENT_OPEN_QUESTION)
	if err != nil {
		log.Printf("Failed to get answer: %v", err)
		return
	}

	log.Printf("eventOpen reply: %v", getStr(answer.(JsonTable), "text"))
}

func eventClose(message JsonTable) {
	if !authorize(message) {
		return
	}
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
	sendPrivateMessage(getNum(getTbl(message, "from"), "id"), HELP_MESSAGE, false)
}

func whoAmI(message JsonTable) {
	chatId := getNum(getTbl(message, "chat"), "id")
	messageId := getNum(message, "message_id")
	resp, err := tgApiCall("getChatMember",
		JsonTable{
			"chat_id": chatId,
			"user_id": getNum(getTbl(message, "from"), "id"),
		})
	if err == nil {
		sendReply(chatId, messageId, "```\n"+toJson(resp)+"```")
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

		delimIdx := strings.Index(text, "@")
		if delimIdx != -1 {
			botName := text[delimIdx+1:]
			if botName != bot_name {
				return
			}
			text = text[:delimIdx]
		}
		log.Printf("text %s", text)

		handler, ok := commandHandlers[text]
		if ok {
			handler(message)
		}
	}
}

func main() {
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
