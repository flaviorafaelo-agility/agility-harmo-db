package main

import (
	"bufio"
	"encoding/binary"
	"log"
	"net"
)

func mainX() {
	listener, err := net.Listen("tcp", ":5430")
	if err != nil {
		log.Fatal("Erro ao iniciar:", err)
	}
	log.Println("Servidor PostgreSQL mock rodando na porta 5430...")

	for {
		conn, _ := listener.Accept()
		go handleConnection(conn)
	}
}

func handleConnection(conn net.Conn) {

	defer conn.Close()
	r := bufio.NewReader(conn)

	// Etapa SSL
	_, code := readInt32(r), readInt32(r)
	if code == 80877103 {
		conn.Write([]byte("N"))
	} else {
		log.Println("Não é SSLRequest")
		return
	}

	// Startup
	length := readInt32(r)
	startup := make([]byte, length-4)
	r.Read(startup)

	// Autenticação
	sendAuthCleartextPassword(conn)
	if readByte(r) != 'p' {
		log.Println("Esperado tag 'p'")
		return
	}
	pwdLen := readInt32(r)
	password := make([]byte, pwdLen-4)
	r.Read(password)
	log.Println("Senha recebida:", string(password))
	sendAuthOK(conn)

	// Parâmetros iniciais
	sendParameterStatus(conn, "server_version", "15.3")
	sendParameterStatus(conn, "client_encoding", "UTF8")
	sendParameterStatus(conn, "DateStyle", "ISO, MDY")
	sendParameterStatus(conn, "integer_datetimes", "on")
	sendParameterStatus(conn, "TimeZone", "UTC")
	sendBackendKeyData(conn, 1, 1)
	sendReadyForQuery(conn)

	// Loop principal
	for {
		tag, err := r.ReadByte()
		if err != nil {
			log.Println("Conexão encerrada:", err)
			return
		}

		switch tag {

		case 'Q': // Query simples
			qLen := readInt32(r)
			query := make([]byte, qLen-4)
			r.Read(query)
			sql := string(query)
			log.Println("Consulta (modo simples):", sql)

			switch {
			case sql == "SET extra_float_digits = 3":
				sendCommandComplete(conn, "SET")
				sendReadyForQuery(conn)

			case sql == "SELECT * FROM pg_catalog.pg_tables;":
				sendRowDescriptionTableList(conn)
				sendDataRow(conn, []string{"clientes"})
				sendDataRow(conn, []string{"produtos"})
				sendDataRow(conn, []string{"vendas"})
				sendCommandComplete(conn, "SELECT 3")
				sendReadyForQuery(conn)

			default:
				sendRowDescriptionCustom(conn, []string{"resultado"})
				sendDataRow(conn, []string{"Hello, AgilityDB!"})
				sendCommandComplete(conn, "SELECT 1")
				sendReadyForQuery(conn)
			}

		case 'P':
			readAndDiscard(r)
			log.Println("Parse recebido")
			conn.Write([]byte{'1'})
			writeInt32(conn, 4)

		case 'B':
			readAndDiscard(r)
			log.Println("Bind recebido")
			conn.Write([]byte{'2'})
			writeInt32(conn, 4)

		case 'C':
			readAndDiscard(r)
			log.Println("Close recebido")
			conn.Write([]byte{'3'})
			writeInt32(conn, 4)

		case 'D': // Describe
			length := readInt32(r)
			payload := make([]byte, length-4)
			r.Read(payload)
			log.Println("Describe recebido")
			sendRowDescription(conn) // <-- essencial!

		case 'E': // Execute
			length := readInt32(r)
			r.Discard(int(length - 4))
			log.Println("Execute recebido")

			sendDataRow(conn, []string{"Hello from Execute"})
			sendCommandComplete(conn, "SELECT 1")
			sendReadyForQuery(conn)

		case 'S', 's':
			readAndDiscard(r)
			log.Println("Sync recebido")
			sendReadyForQuery(conn)

		case 'X':
			log.Println("Comando Terminate recebido (X). Encerrando conexão.")
			return

		default:
			log.Printf("Comando não suportado: %c\n", tag)
			return
		}
	}
}

func sendRowDescription(conn net.Conn) {
	buf := []byte{0x00, 0x01}
	buf = append(buf, []byte("result")...)
	buf = append(buf, 0x00)
	buf = append(buf, 0, 0, 0, 0)
	buf = append(buf, 0, 0)
	buf = append(buf, 0, 0, 0, 25)
	buf = append(buf, 0xFF, 0xFF)
	buf = append(buf, 0xFF, 0xFF, 0xFF, 0xFF)
	buf = append(buf, 0, 0)

	conn.Write([]byte{'T'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}

func sendDataRowX(conn net.Conn, values []string) {
	buf := []byte{0, 1}
	for _, val := range values {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(len(val)))
		buf = append(buf, b...)
		buf = append(buf, []byte(val)...)
	}
	conn.Write([]byte{'D'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}

func sendDataRow(conn net.Conn, values []string) {
	// Quantidade de colunas (2 bytes)
	buf := []byte{byte(len(values) >> 8), byte(len(values))}

	for _, val := range values {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(len(val)))
		buf = append(buf, b...)
		buf = append(buf, []byte(val)...)
	}

	conn.Write([]byte{'D'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}

func sendCommandComplete(conn net.Conn, tag string) {
	buf := append([]byte(tag), 0)
	conn.Write([]byte{'C'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}

func sendAuthCleartextPassword(conn net.Conn) {
	conn.Write([]byte{'R'})
	writeInt32(conn, 8)
	writeInt32(conn, 3)
}

func sendAuthOK(conn net.Conn) {
	conn.Write([]byte{'R'})
	writeInt32(conn, 8)
	writeInt32(conn, 0)
}

func sendParameterStatus(conn net.Conn, name, value string) {
	conn.Write([]byte{'S'})
	payload := []byte(name + "\x00" + value + "\x00")
	writeInt32(conn, int32(len(payload)+4))
	conn.Write(payload)
}

func sendBackendKeyData(conn net.Conn, pid, secretKey int32) {
	conn.Write([]byte{'K'})
	writeInt32(conn, 12)
	writeInt32(conn, pid)
	writeInt32(conn, secretKey)
}

func sendReadyForQuery(conn net.Conn) {
	conn.Write([]byte{'Z'})
	writeInt32(conn, 5)
	conn.Write([]byte{'I'})
}

func writeInt32(w net.Conn, val int32) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(val))
	w.Write(b)
}

func readInt32(r *bufio.Reader) int32 {
	b := make([]byte, 4)
	r.Read(b)
	return int32(binary.BigEndian.Uint32(b))
}

func readByte(r *bufio.Reader) byte {
	b, _ := r.ReadByte()
	return b
}

func readAndDiscard(r *bufio.Reader) {
	length := readInt32(r)
	r.Discard(int(length - 4))
}

func sendNoData(conn net.Conn) {
	conn.Write([]byte{'n'})
	writeInt32(conn, 4)
}

func sendRowDescriptionTableList(conn net.Conn) {
	buf := []byte{0x00, 0x01}
	buf = append(buf, []byte("relname")...) // nome da coluna
	buf = append(buf, 0x00)
	buf = append(buf, 0, 0, 0, 0)
	buf = append(buf, 0, 0)
	buf = append(buf, 0, 0, 0, 25) // OID do tipo TEXT
	buf = append(buf, 0xFF, 0xFF)
	buf = append(buf, 0xFF, 0xFF, 0xFF, 0xFF)
	buf = append(buf, 0, 0)
	conn.Write([]byte{'T'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}

func sendRowDescriptionCustom(conn net.Conn, cols []string) {
	buf := []byte{}
	buf = append(buf, byte(len(cols)>>8), byte(len(cols))) // número de colunas

	for _, col := range cols {
		buf = append(buf, []byte(col)...)
		buf = append(buf, 0x00)
		buf = append(buf, 0, 0, 0, 0)             // tableOID
		buf = append(buf, 0, 0)                   // column index
		buf = append(buf, 0, 0, 0, 25)            // typeOID = TEXT
		buf = append(buf, 0xFF, 0xFF)             // size
		buf = append(buf, 0xFF, 0xFF, 0xFF, 0xFF) // modifier
		buf = append(buf, 0, 0)                   // formatCode
	}

	conn.Write([]byte{'T'})
	writeInt32(conn, int32(len(buf)+4))
	conn.Write(buf)
}
