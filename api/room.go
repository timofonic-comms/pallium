package api

import (
	"encoding/json"
	c "github.com/KoFish/pallium/config"
	m "github.com/KoFish/pallium/matrix"
	s "github.com/KoFish/pallium/storage"
	"io"
	"log"
	"strings"
)

func ListPublicRooms(user *s.User, req io.Reader, vars Vars, query Query) (interface{}, error) {
	var (
		res struct {
			Chunk []s.Room `json:"chunk"`
			End   string   `json:"end"`
			Start string   `json:"start"`
		}
	)
	res.Start = "START"
	res.End = "END"
	db := s.GetDatabase()
	rooms := s.GetPublicRooms(db)
	res.Chunk = rooms
	return res, nil
}

func JoinRoom(user *s.User, req io.Reader, vars Vars, query Query) (interface{}, error) {
	var (
		request  struct{}
		response struct {
			RoomID string `json:"room_id"`
		}
		room_id m.RoomID
	)
	q_room := vars["room"]
	if err := json.NewDecoder(req).Decode(request); err != nil {
		return nil, ENotJSON(err.Error())
	}
	if room_alias, err := m.ParseRoomAlias(q_room); err != nil {
		if room_id, err = m.ParseRoomID(q_room); err != nil {
			log.Println(err.Error())
			return nil, EBadJSON("Could not parse room identifier")
		}
	} else {
		db := s.GetDatabase()
		if room_id, _, err = s.LookupRoomAlias(db, room_alias); err != nil {
			log.Println(err.Error())
			return nil, ENotFound("Could not resolve room alias")
		}
	}
	db := s.GetDatabase()
	tx, err := db.Begin()
	if err != nil {
		log.Println(err.Error())
		panic("matrix: could not start database transaction")
	}
	room := s.Room{ID: room_id}
	if err := room.CheckedUpdateMember(tx, user.UserID, user.UserID, m.MEMBERSHIP_JOIN); err != nil {
		tx.Rollback()
		log.Println(err.Error())
		return nil, EForbidden("Could not join room")
	}
	tx.Commit()
	response.RoomID = room_id.String()
	return response, nil
}

func CreateRoom(user *s.User, req io.Reader, vars Vars, query Query) (interface{}, error) {
	var (
		request struct {
			Visibility    string   `json:"visibility,omitempty"`
			RoomAliasName string   `json:"room_alias_name,omitempty"`
			Name          string   `json:"name,omitempty"`
			Topic         string   `json:"topic,omitempty"`
			Invite        []string `json:"invite,omitempty"`
		}
		response struct {
			RoomID    string `json:"room_id"`
			RoomAlias string `json:"room_alias,omitempty"`
		}
	)
	if err := json.NewDecoder(req).Decode(&request); err != nil {
		return nil, ENotJSON(err.Error())
	}
	var (
		is_public  bool           = false
		join_rule  m.RoomJoinRule = m.JOIN_PUBLIC
		room_alias *m.RoomAlias   = nil
		invitees   []m.UserID     = []m.UserID{}
	)
	/// VALIDATE VISIBILITY
	switch strings.ToLower(request.Visibility) {
	case "public":
		is_public = true
	case "private":
		is_public = false
	case "":
	default:
		return nil, EBadJSON("Unknown room visibility setting")
	}
	/// VALIDATE ROOM ALIAS
	// TODO(): The alias is only supposed to be the localpart.
	if request.RoomAliasName != "" {
		alias, err := m.ParseRoomAlias(request.RoomAliasName)
		if err != nil {
			log.Println(err.Error())
			return nil, EBadJSON("Incorrectly formatted room alias")
		} else {
			room_alias = &alias
		}
	}
	/// VALIDATE INVITES
	if len(request.Invite) > 0 {
		for _, invitee := range request.Invite {
			target, err := m.ParseUserID(invitee)
			if err != nil {
				log.Println(err.Error())
				return nil, EBadJSON("Incorrectly formatted User ID to invite")
			} else {
				invitees = append(invitees, target)
			}
		}
	}
	/// Do database transactions
	db := s.GetDatabase()
	if tx, err := db.Begin(); err != nil {
		panic("matrix: could not open database transaction")
	} else {
		if room, err := s.CreateRoom(tx, user.UserID, is_public); err != nil {
			tx.Rollback()
			log.Println(err.Error())
			log.Fatal("matrix: could not create room")
		} else {
			if err = room.UpdateMember(tx, user.UserID, user.UserID, m.MEMBERSHIP_JOIN); err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not join room creator to the room")
			}
			err = room.UpdateJoinRule(tx, user.UserID, join_rule)
			if err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not set new rooms join rule")
			}
			default_power_level := c.Config.DefaultPowerLevel
			default_ops_levels := map[string]int64{
				"ban":    default_power_level,
				"kick":   default_power_level,
				"redact": default_power_level,
			}
			creator_power_level := map[m.UserID]int64{
				user.UserID: c.Config.DefaultCreatorPowerLevel,
			}
			if err = room.UpdatePowerLevels(tx, user.UserID, creator_power_level, &default_power_level); err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not set power levels for creator")
			}
			if err = room.UpdateAddStateLevel(tx, user.UserID, c.Config.DefaultPowerLevel); err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not set add state level")
			}
			if err = room.UpdateSendEventLevel(tx, user.UserID, c.Config.DefaultPowerLevel); err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not set send event level")
			}
			if err = room.UpdateOpsLevel(tx, user.UserID, default_ops_levels); err != nil {
				tx.Rollback()
				log.Println(err.Error())
				return nil, EForbidden("Could not set ops level")
			}
			if room_alias != nil {
				alias, err := m.NewRoomAlias(room_alias.Localpart(), room_alias.Domain())
				if err = room.UpdateAliases(tx, user.UserID, []m.RoomAlias{alias}); err != nil {
					tx.Rollback()
					log.Println(err.Error())
					return nil, EForbidden("Could not set room alias")
				}
				response.RoomAlias = alias.String()
			}
			if request.Topic != "" {
				if err = room.UpdateTopic(tx, user.UserID, request.Topic); err != nil {
					tx.Rollback()
					log.Println(err.Error())
					return nil, EForbidden("Could not set room topic")
				}
			}
			if request.Name != "" {
				if err = room.UpdateName(tx, user.UserID, request.Name); err != nil {
					tx.Rollback()
					log.Println(err.Error())
					return nil, EForbidden("Could not set room name")
				}
			}
			for _, invitee := range invitees {
				if room.InviteMember(tx, user.UserID, invitee); err != nil {
					tx.Rollback()
					log.Println(err.Error())
					return nil, EForbidden("Could not invite member")
				}
			}
			response.RoomID = room.ID.String()
			tx.Commit()
		}
	}
	return response, nil
}

/// From the rest/rooms.go file

// func roomAliasLookup(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req  struct{}
//         resp struct {
//             RoomID  string   `json:"room_id"`
//             Servers []string `json:"servers"`
//         }
//     )
//     vars := mux.Vars(r)
//     q_room, _ := vars["roomalias"]
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     if room_alias, err := m.ParseRoomAlias(q_room); err != nil {
//         return nil, u.NewError(m.M_FORBIDDEN, "Not a valid room alias")
//     } else {
//         room_id, servers, err := s.LookupRoomAlias(db, room_alias)
//         if err != nil {
//             return nil, u.NewError(m.M_FORBIDDEN, "Unknown room alias")
//         } else {
//             resp.RoomID = room_id.String()
//             resp.Servers = servers
//             return resp, nil
//         }
//     }
// }

// func roomAliasCreate(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req struct {
//             RoomID string `json:"room_id"`
//         }
//         resp struct {
//             RoomID  string   `json:"room_id"`
//             Servers []string `json:"servers"`
//         }
//         room_id m.RoomID
//     )
//     vars := mux.Vars(r)
//     q_room, _ := vars["roomalias"]
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     if room_alias, err := m.ParseRoomAlias(q_room); err != nil {
//         return nil, u.NewError(m.M_FORBIDDEN, "Not a valid room alias")
//     } else {
//         return nil, u.NewError(m.M_FORBIDDEN, "Not allowed to delete alias")
//     }
// }

// func roomAliasDelete(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req  struct{}
//         resp struct {
//             RoomID string `json:"room_id"`
//         }
//         room_id m.RoomID
//     )
//     vars := mux.Vars(r)
//     q_room, _ := vars["roomalias"]
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     if room_alias, err := m.ParseRoomAlias(q_room); err != nil {
//         return nil, u.NewError(m.M_FORBIDDEN, "Not a valid room alias")
//     } else {
//         return nil, u.NewError(m.M_FORBIDDEN, "Not allowed to delete alias")
//     }
// }

// func leaveRoom(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req  leaveRoomRequest
//         resp leaveRoomResponse
//     )
//     if !u.CheckTxnId(r) {
//         return nil, u.NewError(m.M_FORBIDDEN, "Request has already been sent")
//     }
//     vars := mux.Vars(r)
//     q_room := r.URL.Query().Get("room")
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     resp = leaveRoomResponse{rid}
//     return resp, nil
// }

// func inviteRoom(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req  inviteRoomRequest
//         resp inviteRoomResponse
//     )
//     if !u.CheckTxnId(r) {
//         return nil, u.NewError(m.M_FORBIDDEN, "Request has already been sent")
//     }
//     vars := mux.Vars(r)
//     q_room := r.URL.Query().Get("room")
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     resp = inviteRoomResponse{rid}
//     return resp, nil
// }

// func banRoom(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//     var (
//         req  banRoomRequest
//         resp banRoomResponse
//     )
//     if !u.CheckTxnId(r) {
//         return nil, u.NewError(m.M_FORBIDDEN, "Request has already been sent")
//     }
//     vars := mux.Vars(r)
//     q_room := r.URL.Query().Get("room")
//     body, err := ioutil.ReadAll(r.Body)
//     if err != nil {
//         return nil, err
//     }
//     if err = json.Unmarshal(body, &req); err != nil {
//         return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//     }
//     resp = banRoomResponse{rid}
//     return resp, nil
// }
//
//func setRoomState(db *sql.DB, user *s.User, w http.ResponseWriter, r *http.Request) (interface{}, error) {
//    var (
//        req  roomStateRequest
//        resp roomStateResponse
//    )
//    if !u.CheckTxnId(r) {
//        return nil, u.NewError(m.M_FORBIDDEN, "Request has already been sent")
//    }
//    vars := mux.Vars(r)
//    q_room := r.URL.Query().Get("room")
//    q_state_type := r.URL.Query().Get("state_type")
//    q_state_key := r.URL.Query().Get("state_key")
//    body, err := ioutil.ReadAll(r.Body)
//    if err != nil {
//        return nil, err
//    }
//    if err = json.Unmarshal(body, &req); err != nil {
//        return nil, u.NewError(m.M_NOT_JSON, "Could not parse json: "+err.Error())
//    }
//    room_id, err := m.ParseRoomID(q_room)
//    if err != nil {
//        return nil, u.NewError(m.M_FORBIDDEN, "Room is not a valid room ID")
//    }
//    room, err := s.GetRoom(room_id)
//    if err != nil {
//        return nil, u.NewError(m.M_FORBIDDEN, "No such room exists")
//    }
//    resp = roomStateResponse{rid}
//    return resp, nil
//}
