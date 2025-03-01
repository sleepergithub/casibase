// Copyright 2023 The casbin Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"encoding/json"
	"fmt"

	"github.com/casibase/casibase/object"
	"github.com/casibase/casibase/util"
)

func (c *ApiController) GetGlobalMessages() {
	messages, err := object.GetGlobalMessages()
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(messages)
}

func (c *ApiController) GetMessages() {
	owner := c.Input().Get("owner")
	chat := c.Input().Get("chat")

	if chat == "" {
		messages, err := object.GetMessages(owner)
		if err != nil {
			c.ResponseError(err.Error())
			return
		}
		c.ResponseOk(messages)
		return
	}

	messages, err := object.GetChatMessages(chat)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(messages)
}

func (c *ApiController) GetMessage() {
	id := c.Input().Get("id")

	message, err := object.GetMessage(id)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(message)
}

func (c *ApiController) GetMessageAnswer() {
	id := c.Input().Get("id")

	c.Ctx.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
	c.Ctx.ResponseWriter.Header().Set("Cache-Control", "no-cache")
	c.Ctx.ResponseWriter.Header().Set("Connection", "keep-alive")

	message, err := object.GetMessage(id)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	if message == nil {
		c.ResponseErrorStream(fmt.Sprintf("The message: %s is not found", id))
		return
	}

	if message.Author != "AI" || message.ReplyTo == "" || message.Text != "" {
		c.ResponseErrorStream("The message is invalid")
		return
	}

	chatId := util.GetIdFromOwnerAndName(message.Owner, message.Chat)
	chat, err := object.GetChat(chatId)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	//if chat == nil || chat.Organization != message.Organization {
	//	c.ResponseErrorStream(fmt.Sprintf("The chat: %s is not found", chatId))
	//	return
	//}

	if chat.Type != "AI" {
		c.ResponseErrorStream("The chat type must be \"AI\"")
		return
	}

	store, err := object.GetDefaultStore("admin")
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}
	if store == nil {
		c.ResponseErrorStream(fmt.Sprintf("The default store is not found"))
		return
	}

	question := store.Welcome
	if message.ReplyTo != "Welcome" {
		questionMessage, err := object.GetMessage(message.ReplyTo)
		if err != nil {
			c.ResponseErrorStream(err.Error())
			return
		}
		if questionMessage == nil {
			c.ResponseErrorStream(fmt.Sprintf("The message: %s is not found", id))
			return
		}

		question = questionMessage.Text
	}

	if question == "" {
		c.ResponseErrorStream(fmt.Sprintf("The question should not be empty"))
		return
	}

	_, ok := c.CheckSignedIn()
	if !ok {
		var count int
		count, err = object.GetNearMessageCount(message.User, store.LimitMinutes)
		if err != nil {
			c.ResponseErrorStream(err.Error())
			return
		}
		if count > store.Frequency {
			c.ResponseErrorStream("You have queried too many times, please wait for a while")
			return
		}
	}

	_, modelProviderObj, err := getModelProviderFromContext("admin", chat.User2)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	embeddingProvider, embeddingProviderObj, err := getEmbeddingProviderFromContext("admin", chat.User2)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	c.Ctx.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
	c.Ctx.ResponseWriter.Header().Set("Cache-Control", "no-cache")
	c.Ctx.ResponseWriter.Header().Set("Connection", "keep-alive")

	knowledge, vectorScores, err := object.GetNearestKnowledge(embeddingProvider, embeddingProviderObj, "admin", question)
	if err != nil && err.Error() != "no knowledge vectors found" {
		c.ResponseErrorStream(err.Error())
		return
	}

	writer := &RefinedWriter{*c.Ctx.ResponseWriter, *NewCleaner(6), []byte{}}
	history, err := object.GetRecentRawMessages(chat.Name, store.MemoryLimit)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	fmt.Printf("Question: [%s]\n", question)
	fmt.Printf("Knowledge: [\n")
	for i, k := range knowledge {
		fmt.Printf("Knowledge %d: [%s]\n", i, k.Text)
	}
	fmt.Printf("]\n")
	// fmt.Printf("Refined Question: [%s]\n", realQuestion)
	fmt.Printf("Answer: [")

	err = modelProviderObj.QueryText(question, writer, history, store.Prompt, knowledge)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}
	if writer.writerCleaner.cleaned == false {
		cleanedData := writer.writerCleaner.GetCleanedData()
		writer.buf = append(writer.buf, []byte(cleanedData)...)
		jsonData, err := ConvertMessageDataToJSON(cleanedData)
		if err != nil {
			c.ResponseErrorStream(err.Error())
			return
		}
		_, err = writer.ResponseWriter.Write([]byte(fmt.Sprintf("event: message\ndata: %s\n\n", jsonData)))
		if err != nil {
			c.ResponseErrorStream(err.Error())
			return
		}
		writer.Flush()
		fmt.Print(cleanedData)
	}

	fmt.Printf("]\n")

	event := fmt.Sprintf("event: end\ndata: %s\n\n", "end")
	_, err = c.Ctx.ResponseWriter.Write([]byte(event))
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}

	answer := writer.String()

	message.Text = answer
	message.VectorScores = vectorScores
	_, err = object.UpdateMessage(message.GetId(), message)
	if err != nil {
		c.ResponseErrorStream(err.Error())
		return
	}
}

func (c *ApiController) UpdateMessage() {
	id := c.Input().Get("id")

	var message object.Message
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	success, err := object.UpdateMessage(id, &message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(success)
}

func (c *ApiController) AddMessage() {
	var message object.Message
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	var chat *object.Chat
	if message.Chat != "" {
		chatId := util.GetId(message.Owner, message.Chat)
		chat, err = object.GetChat(chatId)
		if err != nil {
			c.ResponseError(err.Error())
			return
		}

		if chat == nil {
			c.ResponseError(fmt.Sprintf("chat:The chat: %s is not found", chatId))
			return
		}
	}

	host := c.Ctx.Request.Host
	origin := getOriginFromHost(host)
	err = object.RefineMessageImages(&message, origin)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	message.CreatedTime = util.GetCurrentTimeWithMilli()

	success, err := object.AddMessage(&message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	if success {
		if chat != nil && chat.Type == "AI" {
			answerMessage := &object.Message{
				Owner:       message.Owner,
				Name:        fmt.Sprintf("message_%s", util.GetRandomName()),
				CreatedTime: util.GetCurrentTimeEx(message.CreatedTime),
				// Organization: message.Organization,
				User:         message.User,
				Chat:         message.Chat,
				ReplyTo:      message.GetId(),
				Author:       "AI",
				Text:         "",
				VectorScores: []object.VectorScore{},
			}
			_, err = object.AddMessage(answerMessage)
			if err != nil {
				c.ResponseError(err.Error())
				return
			}
		}
	}

	c.ResponseOk(success)
}

func (c *ApiController) DeleteMessage() {
	var message object.Message
	err := json.Unmarshal(c.Ctx.Input.RequestBody, &message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	success, err := object.DeleteMessage(&message)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}

	c.ResponseOk(success)
}
