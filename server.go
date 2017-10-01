package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	emailLib "github.com/jordan-wright/email"
	_ "github.com/lib/pq"
)

const (
	contentType     = "Content-Type"
	jsonContentType = "application/json;charset=UTF-8"
)

func NewServer(httpAddr string, db *sql.DB, emailPool *emailLib.Pool) *http.Server {
	// TODO - Add logging middleware
	// TODO - Add secure headers middleware
	r := mux.NewRouter()

	r.HandleFunc("/api/v1/email", CreateEmailAccountHandler(db)).Methods("POST")
	r.HandleFunc("/api/v1/email/{id}/send", SendEmailHandler(db, emailPool)).Methods("POST")
	r.HandleFunc("/api/v1/email/bulksend", SendBulkEmailHandler(db, emailPool)).Methods("POST")
	http.Handle("/", r)

	return &http.Server{
		Addr:    httpAddr,
		Handler: r,
	}
}

func readReqBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, 1048576))
	if err != nil {
		log.Errorf("Error occured when reading r.Body: %s", err)
		return []byte{}, err
	}
	return body, nil
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// Custom version of http.Error to support json error messages
func ErrorRespond(w http.ResponseWriter, errMsg string, code int) {
	resp := &ErrorResponse{Error: errMsg}

	w.Header().Set(contentType, jsonContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Errorf("Error occured when marshalling response: %s", err)
	}
}

type CreateEmailAccountResponse struct {
	Id string `json:"id"`
}

func CreateEmailAccountHandler(db *sql.DB) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		newAccount := &EmailAccount{}
		body, err := readReqBody(r)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(body, newAccount); err != nil {
			log.Errorf("Error occured when unmarshalling data: %s", err)
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		err = newAccount.Save(db)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp := &CreateEmailAccountResponse{
			Id: newAccount.Id,
		}

		w.Header().Set(contentType, jsonContentType)
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Errorf("Error occured when marshalling response: %s", err)
			return
		}
	}
}

type SendEmailRequest struct {
	EmailData  EmailData `json:"email_data"`
	SecureOnly bool      `json:"secure_only,omitempty"`
}

func SendEmailHandler(db *sql.DB, emailPool *emailLib.Pool) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["id"]

		sendEmailReq := &SendEmailRequest{}
		body, err := readReqBody(r)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(body, sendEmailReq); err != nil {
			log.Errorf("Error occured when unmarshalling data: %s", err)
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		// TODO - support returning 500 as well
		emailAccount, err := GetEmailAccount(db, id)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusNotFound)
			return
		}

		if sendEmailReq.SecureOnly && !emailAccount.HasPubKey() {
			errStr := fmt.Sprintf("Failed SecureOnly Email to %s - no pub key", emailAccount.Id)
			log.Warn(errStr)
			ErrorRespond(w, errStr, http.StatusBadRequest)
			return
		}

		err = emailAccount.Send(sendEmailReq.EmailData, emailPool)
		if err != nil {
			log.Errorf("Error sending email: %v", err)
			ErrorRespond(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

type SendBulkEmailRequest struct {
	Ids        []string  `json:"ids"`
	EmailData  EmailData `json:"email_data"`
	SecureOnly bool      `json:"secure_only,omitempty"`
}

type SendBulkEmailResponse struct {
	FailedIds []string `json:"failed_emails"`
}

func SendBulkEmailHandler(db *sql.DB, emailPool *emailLib.Pool) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		sendBulkEmailReq := &SendBulkEmailRequest{}
		body, err := readReqBody(r)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := json.Unmarshal(body, sendBulkEmailReq); err != nil {
			log.Errorf("Error occured when unmarshalling data: %s", err)
			ErrorRespond(w, err.Error(), http.StatusBadRequest)
			return
		}

		// TODO - If SecureOnly is true, should filter out in db query
		// TODO - support returning 500 as well
		emailAccounts, err := GetEmailAccounts(db, sendBulkEmailReq.Ids)
		if err != nil {
			ErrorRespond(w, err.Error(), http.StatusNotFound)
			return
		}

		failedIds := []string{}
		failedIdsChan := make(chan string)
		wg := new(sync.WaitGroup)
		for _, email := range emailAccounts {
			if sendBulkEmailReq.SecureOnly && !email.HasPubKey() {
				failedIds = append(failedIds, email.Id)
				continue
			}

			wg.Add(1)
			go func(email *EmailAccount) {
				defer wg.Done()
				err = email.Send(sendBulkEmailReq.EmailData, emailPool)
				if err != nil {
					log.Errorf("Error sending email: %v", err)
					failedIdsChan <- email.Id
				}
			}(email)
		}

		wg.Wait()
		close(failedIdsChan)
		for failedId := range failedIdsChan {
			failedIds = append(failedIds, failedId)
		}

		if len(failedIds) == 0 {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.Header().Set(contentType, jsonContentType)
			w.WriteHeader(http.StatusCreated)
			resp := &SendBulkEmailResponse{FailedIds: failedIds}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				log.Errorf("Error occured when marshalling response: %s", err)
				return
			}
		}
	}
}
